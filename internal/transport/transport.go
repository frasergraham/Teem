package transport

import (
	"context"
	"errors"
	"io"
)

// Command describes a process to start.
type Command struct {
	Path string
	Args []string
	Env  []string
	Dir  string
}

// Process is a running subprocess. Stdin/Stdout/Stderr are wired before the
// process starts; Wait blocks until exit; Kill terminates the process.
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() io.Reader
	Wait() error
	Kill() error
}

// Transport starts subprocesses on some host. Implementations:
//   - LocalTransport: same host (os/exec).
//   - SSHTransport: remote host via SSH.
type Transport interface {
	Start(ctx context.Context, cmd Command) (Process, error)
}

// ErrNotConfigured is returned by transports that require external setup
// (SSH agent, auth keys) that is missing.
var ErrNotConfigured = errors.New("transport: not configured")
