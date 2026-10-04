package dockerx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// Exec runs cmd in a container and returns its trimmed stdout (at most
// 64 KiB) and exit code.
func Exec(ctx context.Context, docker *client.Client, containerID string, cmd ...string) (string, int, error) {
	created, err := docker.ExecCreate(ctx, containerID, client.ExecCreateOptions{AttachStdout: true, AttachStderr: true, Cmd: cmd})
	if err != nil {
		return "", 0, fmt.Errorf("create exec: %w", err)
	}
	attached, err := docker.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("attach exec: %w", err)
	}
	defer attached.Close()
	stdout := &BoundedBuffer{Limit: 64 << 10}
	stderr := &BoundedBuffer{Limit: 4096}
	copied := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(stdout, stderr, attached.Reader)
		copied <- err
	}()
	select {
	case err := <-copied:
		if err != nil && !errors.Is(err, io.EOF) {
			return "", 0, fmt.Errorf("read exec output: %w", err)
		}
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}
	inspected, err := docker.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("inspect exec: %w", err)
	}
	return strings.TrimSpace(stdout.String()), inspected.ExitCode, nil
}

// BoundedBuffer keeps the first Limit bytes written to it and discards the
// rest. It is safe for concurrent use.
type BoundedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	Limit int
}

func (b *BoundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.Limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *BoundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
