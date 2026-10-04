package builds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// dialBuilder connects to the builder's BuildKit daemon through `buildctl
// dial-stdio` run with Docker exec. The daemon listens only on a Unix socket
// inside its container, so build steps have no network path to it.
func dialBuilder(ctx context.Context, docker *client.Client, containerID string) (net.Conn, error) {
	created, err := docker.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		AttachStdin: true, AttachStdout: true, AttachStderr: true,
		Cmd: []string{"buildctl", "dial-stdio"},
	})
	if err != nil {
		return nil, fmt.Errorf("create builder connection: %w", err)
	}
	attached, err := docker.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("attach builder connection: %w", err)
	}
	return newExecConn(attached.HijackedResponse), nil
}

// execConn is a net.Conn over an exec's stdin and demultiplexed stdout.
type execConn struct {
	hijacked client.HijackedResponse
	stdout   *io.PipeReader
	stderr   *boundedBuffer
	once     sync.Once
}

func newExecConn(h client.HijackedResponse) *execConn {
	reader, writer := io.Pipe()
	c := &execConn{hijacked: h, stdout: reader, stderr: &boundedBuffer{limit: 4096}}
	go func() {
		_, err := stdcopy.StdCopy(writer, c.stderr, h.Reader)
		if err == nil {
			err = io.EOF
		}
		writer.CloseWithError(err)
	}()
	return c
}

func (c *execConn) Read(b []byte) (int, error)  { return c.stdout.Read(b) }
func (c *execConn) Write(b []byte) (int, error) { return c.hijacked.Conn.Write(b) }
func (c *execConn) Close() error {
	c.once.Do(func() {
		_ = c.hijacked.CloseWrite()
		c.hijacked.Close()
		_ = c.stdout.Close()
	})
	return nil
}
func (c *execConn) LocalAddr() net.Addr                { return c.hijacked.Conn.LocalAddr() }
func (c *execConn) RemoteAddr() net.Addr               { return c.hijacked.Conn.RemoteAddr() }
func (c *execConn) SetDeadline(t time.Time) error      { return c.hijacked.Conn.SetDeadline(t) }
func (c *execConn) SetReadDeadline(t time.Time) error  { return c.hijacked.Conn.SetReadDeadline(t) }
func (c *execConn) SetWriteDeadline(t time.Time) error { return c.hijacked.Conn.SetWriteDeadline(t) }

// boundedBuffer keeps the first limit bytes written to it.
type boundedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// execOutput runs cmd in a container and returns its bounded stdout and exit code.
func execOutput(ctx context.Context, docker *client.Client, containerID string, cmd ...string) (string, int, error) {
	created, err := docker.ExecCreate(ctx, containerID, client.ExecCreateOptions{AttachStdout: true, AttachStderr: true, Cmd: cmd})
	if err != nil {
		return "", 0, fmt.Errorf("create exec: %w", err)
	}
	attached, err := docker.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("attach exec: %w", err)
	}
	defer attached.Close()
	stdout := &boundedBuffer{limit: 64 << 10}
	stderr := &boundedBuffer{limit: 4096}
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
