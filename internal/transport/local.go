package transport

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// LocalTransport runs commands on the current host via os/exec.
type LocalTransport struct{}

func (LocalTransport) Start(ctx context.Context, cmd Command) (Process, error) {
	c := exec.CommandContext(ctx, cmd.Path, cmd.Args...)
	c.Env = cmd.Env
	c.Dir = cmd.Dir
	stdin, err := c.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("local: stdin: %w", err)
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("local: stdout: %w", err)
	}
	stderr, err := c.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("local: stderr: %w", err)
	}
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("local: start: %w", err)
	}
	return &localProcess{cmd: c, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

type localProcess struct {
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	stdout, stderr io.Reader
}

func (p *localProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *localProcess) Stdout() io.Reader     { return p.stdout }
func (p *localProcess) Stderr() io.Reader     { return p.stderr }
func (p *localProcess) Wait() error           { return p.cmd.Wait() }
func (p *localProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}
