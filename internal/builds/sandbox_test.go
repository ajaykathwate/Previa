package builds

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/client"
)

func TestStoppedBuilderReportsStartupDiagnostics(t *testing.T) {
	for _, tt := range []struct {
		name     string
		logs     string
		logsFail bool
		want     string
	}{
		{
			name: "namespace restriction",
			logs: "rootlesskit: fork/exec /proc/self/exe: permission denied\n" +
				"kernel.apparmor_restrict_unprivileged_userns is set to 1\n",
			want: "kernel.apparmor_restrict_unprivileged_userns is set to 1",
		},
		{
			name: "bounded daemon output",
			logs: "startup failed\n" + strings.Repeat("x", 8192),
			want: "startup failed",
		},
		{
			name:     "logs unavailable",
			logsFail: true,
			want:     "daemon logs unavailable",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/containers/builder/json"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"Id":"builder","State":{"Status":"exited","Running":false,"ExitCode":1}}`)
				case strings.HasSuffix(r.URL.Path, "/containers/builder/logs"):
					if r.URL.Query().Get("tail") != "20" {
						t.Errorf("logs must be bounded to recent startup output: %s", r.URL.RawQuery)
					}
					if tt.logsFail {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = io.WriteString(w, `{"message":"daemon logs unavailable"}`)
						return
					}
					w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
					header := make([]byte, 8)
					header[0] = 2 // Docker's stderr stream.
					binary.BigEndian.PutUint32(header[4:], uint32(len(tt.logs)))
					_, _ = w.Write(header)
					_, _ = io.WriteString(w, tt.logs)
				default:
					t.Errorf("stopped builder must fail without readiness exec: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			docker, err := client.New(client.WithHost(server.URL), client.WithAPIVersion("1.56"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = docker.Close() }()

			sb := sandbox{docker: docker, containerID: "builder"}
			err = sb.waitReady(t.Context())
			if err == nil || !strings.Contains(err.Error(), "exit code 1") || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("startup failure lost its cause or daemon diagnostics: %v", err)
			}
			if len(err.Error()) > 4600 {
				t.Fatalf("startup diagnostics grew beyond the bounded log output: %d bytes", len(err.Error()))
			}
		})
	}
}

func TestBuilderReadinessPreservesCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("cancelled startup must not make Docker requests: %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	docker, err := client.New(client.WithHost(server.URL), client.WithAPIVersion("1.56"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = docker.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	sb := sandbox{docker: docker, containerID: "builder"}
	if err := sb.waitReady(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("startup cancellation was not preserved: %v", err)
	}
}
