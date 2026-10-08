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
	result, err := Run(ctx, docker, containerID, cmd...)
	return result.Stdout, result.ExitCode, err
}

// ExecResult is a finished command's trimmed output and exit code.
type ExecResult struct {
	Stdout   string // at most 64 KiB
	Stderr   string // at most 4 KiB
	ExitCode int
}

// Run runs cmd in a container and returns its output and exit code.
func Run(ctx context.Context, docker *client.Client, containerID string, cmd ...string) (ExecResult, error) {
	return RunWithEnv(ctx, docker, containerID, nil, cmd...)
}

// RunWithEnv runs cmd with extra environment variables, which keep values
// such as a password out of the command line.
func RunWithEnv(ctx context.Context, docker *client.Client, containerID string, env []string, cmd ...string) (ExecResult, error) {
	created, err := docker.ExecCreate(ctx, containerID, client.ExecCreateOptions{AttachStdout: true, AttachStderr: true, Env: env, Cmd: cmd})
	if err != nil {
		return ExecResult{}, fmt.Errorf("create exec: %w", err)
	}
	attached, err := docker.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return ExecResult{}, fmt.Errorf("attach exec: %w", err)
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
			return ExecResult{}, fmt.Errorf("read exec output: %w", err)
		}
	case <-ctx.Done():
		return ExecResult{}, ctx.Err()
	}
	inspected, err := docker.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return ExecResult{}, fmt.Errorf("inspect exec: %w", err)
	}
	return ExecResult{Stdout: strings.TrimSpace(stdout.String()), Stderr: strings.TrimSpace(stderr.String()), ExitCode: inspected.ExitCode}, nil
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
