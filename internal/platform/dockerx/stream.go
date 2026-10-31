package dockerx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// Streams move large outputs, such as a database dump, between containers
// and the worker without holding them in memory: one command's standard
// output is read as a stream, another command's standard input is fed from
// one. Neither runs a shell unless cmd does.

// OutputStream is a running command's standard output.
type OutputStream struct {
	io.Reader
	docker *client.Client
	execID string
	conn   client.HijackedResponse
	copied chan error
	stderr *BoundedBuffer
}

// StreamOutput runs cmd in a container and returns its standard output as a
// stream. Read it to the end, then Wait for the command's result.
func StreamOutput(ctx context.Context, docker *client.Client, containerID string, cmd ...string) (*OutputStream, error) {
	created, err := docker.ExecCreate(ctx, containerID, client.ExecCreateOptions{AttachStdout: true, AttachStderr: true, Cmd: cmd})
	if err != nil {
		return nil, fmt.Errorf("create exec: %w", err)
	}
	attached, err := docker.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("attach exec: %w", err)
	}
	reader, writer := io.Pipe()
	s := &OutputStream{Reader: reader, docker: docker, execID: created.ID, conn: attached.HijackedResponse, copied: make(chan error, 1), stderr: &BoundedBuffer{Limit: 4096}}
	go func() {
		_, err := stdcopy.StdCopy(writer, s.stderr, attached.Reader)
		writer.CloseWithError(err)
		s.copied <- err
	}()
	return s, nil
}

// Wait closes the stream and returns the command's exit code and the start
// of its standard error. A command still running is waited for, until ctx
// ends.
func (s *OutputStream) Wait(ctx context.Context) (ExecResult, error) {
	s.conn.Close()
	select {
	case <-s.copied:
	case <-ctx.Done():
		return ExecResult{}, ctx.Err()
	}
	code, err := exitCode(ctx, s.docker, s.execID)
	return ExecResult{Stderr: s.stderr.String(), ExitCode: code}, err
}

// FeedInput runs cmd in a container with input as its standard input and
// waits until it exits. It returns the command's exit code and the start of
// its output, and the error reading input, if any: a command can exit early,
// so both matter.
func FeedInput(ctx context.Context, docker *client.Client, containerID string, input io.Reader, cmd ...string) (ExecResult, error) {
	created, err := docker.ExecCreate(ctx, containerID, client.ExecCreateOptions{AttachStdin: true, AttachStdout: true, AttachStderr: true, Cmd: cmd})
	if err != nil {
		return ExecResult{}, fmt.Errorf("create exec: %w", err)
	}
	attached, err := docker.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return ExecResult{}, fmt.Errorf("attach exec: %w", err)
	}
	defer attached.Close()
	stdout := &BoundedBuffer{Limit: 4096}
	stderr := &BoundedBuffer{Limit: 4096}
	copied := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(stdout, stderr, attached.Reader)
		copied <- err
	}()
	fed := make(chan error, 1)
	go func() {
		_, err := io.Copy(attached.Conn, input)
		closeErr := attached.CloseWrite()
		fed <- errors.Join(err, closeErr)
	}()
	var feedErr error
	select {
	case feedErr = <-fed:
	case <-ctx.Done():
		return ExecResult{}, ctx.Err()
	}
	select {
	case <-copied:
	case <-ctx.Done():
		return ExecResult{}, ctx.Err()
	}
	code, err := exitCode(ctx, docker, created.ID)
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}, feedErr
}

// exitCode waits briefly for an exec whose output ended to be reported as
// exited, then returns its exit code.
func exitCode(ctx context.Context, docker *client.Client, execID string) (int, error) {
	for range 50 {
		inspected, err := docker.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		if err != nil {
			return 0, fmt.Errorf("inspect exec: %w", err)
		}
		if !inspected.Running {
			return inspected.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return 0, errors.New("exec did not exit after its output ended")
}
