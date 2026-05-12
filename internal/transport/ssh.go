package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// SSHTransport runs commands on a remote host over SSH using the local
// ssh-agent for authentication. Auth via password or key files is not
// supported in v1 — set SSH_AUTH_SOCK and load your key into the agent.
type SSHTransport struct {
	// Target is `[user@]host[:port]`.
	Target string
	// HostKeyCallback may be supplied for production; defaults to
	// ssh.InsecureIgnoreHostKey() if nil. v1 ignores host keys to keep the
	// path simple; production deployments should set this.
	HostKeyCallback ssh.HostKeyCallback
}

func (t SSHTransport) Start(ctx context.Context, cmd Command) (Process, error) {
	user, host, port := parseTarget(t.Target)
	if host == "" {
		return nil, fmt.Errorf("ssh: invalid target %q", t.Target)
	}
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, fmt.Errorf("%w: SSH_AUTH_SOCK is empty", ErrNotConfigured)
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("ssh: dial agent: %w", err)
	}
	ag := agent.NewClient(conn)
	hostKeyCB := t.HostKeyCallback
	if hostKeyCB == nil {
		hostKeyCB = ssh.InsecureIgnoreHostKey()
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeysCallback(ag.Signers)},
		HostKeyCallback: hostKeyCB,
	}
	addr := net.JoinHostPort(host, port)
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh: dial %s: %w", addr, err)
	}
	session, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("ssh: new session: %w", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		_ = client.Close()
		return nil, fmt.Errorf("ssh: stdin: %w", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		_ = client.Close()
		return nil, fmt.Errorf("ssh: stdout: %w", err)
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		_ = session.Close()
		_ = client.Close()
		return nil, fmt.Errorf("ssh: stderr: %w", err)
	}
	full := buildRemoteCommand(cmd)
	if err := session.Start(full); err != nil {
		_ = session.Close()
		_ = client.Close()
		return nil, fmt.Errorf("ssh: start: %w", err)
	}
	return &sshProcess{
		session: session,
		client:  client,
		agent:   conn,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
	}, nil
}

func parseTarget(s string) (user, host, port string) {
	port = "22"
	rest := s
	if i := strings.Index(rest, "@"); i != -1 {
		user = rest[:i]
		rest = rest[i+1:]
	}
	if i := strings.LastIndex(rest, ":"); i != -1 {
		host = rest[:i]
		port = rest[i+1:]
	} else {
		host = rest
	}
	return
}

func buildRemoteCommand(cmd Command) string {
	var b strings.Builder
	if cmd.Dir != "" {
		fmt.Fprintf(&b, "cd %s && ", shellQuote(cmd.Dir))
	}
	for _, e := range cmd.Env {
		fmt.Fprintf(&b, "%s ", shellQuote(e))
	}
	b.WriteString(shellQuote(cmd.Path))
	for _, a := range cmd.Args {
		b.WriteByte(' ')
		b.WriteString(shellQuote(a))
	}
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type sshProcess struct {
	session        *ssh.Session
	client         *ssh.Client
	agent          net.Conn
	stdin          io.WriteCloser
	stdout, stderr io.Reader
}

func (p *sshProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *sshProcess) Stdout() io.Reader     { return p.stdout }
func (p *sshProcess) Stderr() io.Reader     { return p.stderr }
func (p *sshProcess) Wait() error {
	err := p.session.Wait()
	_ = p.session.Close()
	_ = p.client.Close()
	_ = p.agent.Close()
	return err
}
func (p *sshProcess) Kill() error {
	// Best-effort: send SIGKILL; SSH server may not honour it.
	_ = p.session.Signal(ssh.SIGKILL)
	_ = p.session.Close()
	_ = p.client.Close()
	_ = p.agent.Close()
	return nil
}
