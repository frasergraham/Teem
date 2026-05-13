package provisioner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/frasergraham/teem/internal/transport"
)

// LocalProvisioner places workers on the current host as detached
// teem-worker processes listening on per-agent unix sockets.
//
// This is the "Approach B" model: a local agent looks exactly like a
// remote one from the daemon's perspective. The daemon talks HTTP to
// the worker over a unix socket; the worker owns its own claude
// subprocess, audit emission, and heartbeats. Workers survive daemon
// stop and reattach on the next start by way of the socket file in
// SocketDir.
//
// Each spawned worker writes its socket under SocketDir, with a
// sidecar pidfile next to it. Teardown POSTs /shutdown and falls back
// to SIGTERM by pid if the worker doesn't respond.
//
// Worktree behavior is unchanged: when WorkingDir is unset and
// RepoRoot+WorktreeBase are set, a git worktree on branch
// teem/<agent-id> is created and used as the worker's cwd.
//
// Persistent local agents are still operator-managed — the daemon
// expects them to already be running at tailnet hostname
// teem-<agent-id> and just returns enough metadata for the spawner
// to point at them.
type LocalProvisioner struct {
	// RepoRoot is the leader's git repo root. Empty disables auto-worktree.
	RepoRoot string
	// WorktreeBase is the directory under which agent worktrees are
	// created. Empty disables auto-worktree.
	WorktreeBase string

	// SocketDir is the directory the spawner uses for per-agent
	// unix sockets and pidfiles. Required for ephemeral local
	// agents; if empty, ephemeral local spawn errors.
	SocketDir string
	// LeaderURL is forwarded to teem-worker as TEEM_LEADER_URL so
	// audit events route back to the daemon.
	LeaderURL string
	// WorkerToken is the shared bearer; passed to teem-worker so the
	// daemon and worker authenticate each other.
	WorkerToken string
	// WorkerBinary is the path to `teem-worker`. Defaults to
	// resolving via PATH at spawn time.
	WorkerBinary string

	// mu serializes git worktree mutations. Concurrent SpawnByRole calls
	// must not race the git index.
	mu sync.Mutex
}

// NewLocalProvisioner constructs a LocalProvisioner. Pass empty
// strings to disable the corresponding subsystem (worktree, socket
// spawning). socketDir is required for ephemeral local agents to be
// spawnable.
func NewLocalProvisioner(repoRoot, worktreeBase string) *LocalProvisioner {
	return &LocalProvisioner{RepoRoot: repoRoot, WorktreeBase: worktreeBase}
}

// NewLocalProvisionerForSubprocess constructs a LocalProvisioner ready
// to fork teem-worker subprocesses. The daemon's buildTeamServices
// uses this.
func NewLocalProvisionerForSubprocess(repoRoot, worktreeBase, socketDir, leaderURL, workerToken string) *LocalProvisioner {
	return &LocalProvisioner{
		RepoRoot:     repoRoot,
		WorktreeBase: worktreeBase,
		SocketDir:    socketDir,
		LeaderURL:    leaderURL,
		WorkerToken:  workerToken,
	}
}

// ErrLocalNoWorkingDir is returned when neither the YAML supplies a
// working_dir nor the leader is running inside a git repo.
var ErrLocalNoWorkingDir = errors.New("local agent requires either working_dir in YAML or that the leader is running inside a git repo")

// ErrLocalNoSocketDir is returned when the operator hasn't configured
// a socket directory for ephemeral local agents.
var ErrLocalNoSocketDir = errors.New("local provisioner has no SocketDir configured")

func (p *LocalProvisioner) Provision(ctx context.Context, spec AgentSpec) (*Agent, error) {
	// Persistent local agents are operator-managed at tailnet
	// hostname teem-<id>. Daemon just returns metadata.
	if spec.IsPersistent() {
		return &Agent{
			ID:          spec.ID,
			Role:        spec.Role,
			Backend:     BackendLocal,
			Lifecycle:   spec.Lifecycle,
			TailnetHost: "teem-" + spec.ID,
			MCPs:        spec.MCPs,
		}, nil
	}

	// Resolve the work directory: explicit YAML wins; otherwise an
	// isolated git worktree on a per-agent branch.
	workDir := spec.WorkingDir
	var branch string
	if workDir == "" {
		if p.RepoRoot == "" || p.WorktreeBase == "" {
			return nil, ErrLocalNoWorkingDir
		}
		workDir = filepath.Join(p.WorktreeBase, spec.ID)
		branch = "teem/" + spec.ID
		p.mu.Lock()
		err := EnsureWorktree(ctx, p.RepoRoot, workDir, branch)
		p.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("local: prepare worktree for %s: %w", spec.ID, err)
		}
	}

	// SocketDir empty → legacy in-process model (a Worker goroutine
	// in the daemon shells out to claude via LocalTransport). The
	// daemon explicitly sets SocketDir for production; tests leave
	// it empty so they don't need a fake teem-worker binary.
	if p.SocketDir == "" {
		a := &Agent{
			ID:         spec.ID,
			Role:       spec.Role,
			WorkingDir: workDir,
			Backend:    BackendLocal,
			Lifecycle:  spec.Lifecycle,
			Transport:  transport.LocalTransport{},
			MCPs:       spec.MCPs,
		}
		if branch != "" {
			a.Cloud = &CloudPlacement{TaskARN: workDir}
		}
		return a, nil
	}

	// Fork the teem-worker subprocess. Detached (setsid) so it
	// outlives the daemon. Stdout/stderr redirect to a per-agent log.
	if err := os.MkdirAll(p.SocketDir, 0o755); err != nil {
		return nil, fmt.Errorf("local: socket dir: %w", err)
	}
	socketPath := filepath.Join(p.SocketDir, spec.ID+".sock")
	pidPath := filepath.Join(p.SocketDir, spec.ID+".pid")
	logPath := filepath.Join(p.SocketDir, spec.ID+".log")
	// Stale socket / pid from a prior unclean exit.
	_ = os.Remove(socketPath)

	binary := p.WorkerBinary
	if binary == "" {
		resolved, err := exec.LookPath("teem-worker")
		if err != nil {
			return nil, fmt.Errorf("local: teem-worker not on PATH: %w", err)
		}
		binary = resolved
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("local: open worker log: %w", err)
	}
	defer logFile.Close()

	env := append(os.Environ(),
		"TEEM_AGENT_ID="+spec.ID,
		"TEEM_AGENT_ROLE="+spec.Role,
		"TEEM_WORKER_TOKEN="+p.WorkerToken,
		"TEEM_WORKER_WORKDIR="+workDir,
		"TEEM_UNIX_SOCKET="+socketPath,
	)
	if p.LeaderURL != "" {
		env = append(env, "TEEM_LEADER_URL="+p.LeaderURL)
	}

	cmd := exec.Command(binary, "--unix-socket", socketPath)
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("local: spawn teem-worker: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release() // detach: we never call Wait()
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		// Best-effort; teardown can still find the worker by socket.
		fmt.Fprintf(os.Stderr, "[local-provisioner] write pidfile %s: %v\n", pidPath, err)
	}

	// Wait for the worker to come up — the socket file appearing is
	// our readiness signal. Up to 5s.
	if err := waitForSocket(socketPath, 5*time.Second); err != nil {
		return nil, fmt.Errorf("local: worker %s did not bind socket: %w", spec.ID, err)
	}

	a := &Agent{
		ID:         spec.ID,
		Role:       spec.Role,
		WorkingDir: workDir,
		Backend:    BackendLocal,
		Lifecycle:  spec.Lifecycle,
		SocketPath: socketPath,
		MCPs:       spec.MCPs,
	}
	if branch != "" {
		// Stash the worktree path on Cloud so Teardown knows what to
		// remove. CloudPlacement is named for the cloud case; we
		// reuse it rather than introduce another struct for a single
		// string.
		a.Cloud = &CloudPlacement{TaskARN: workDir}
	}
	return a, nil
}

func (p *LocalProvisioner) Teardown(ctx context.Context, a *Agent) error {
	if a == nil {
		return nil
	}
	if a.IsPersistent() {
		// Persistent local agents are operator-managed; daemon stop
		// must not touch them.
		return nil
	}

	// Tell the worker to shut down via its socket. Falls back to
	// SIGTERM-by-pid if the HTTP shutdown doesn't take effect.
	if a.SocketPath != "" {
		_ = shutdownWorker(ctx, a.SocketPath, p.WorkerToken)
		_ = killByPidFile(strings.TrimSuffix(a.SocketPath, ".sock") + ".pid")
		_ = os.Remove(a.SocketPath)
	}

	// Worktree cleanup is independent of worker shutdown.
	if a.Cloud != nil && a.Cloud.TaskARN != "" && p.RepoRoot != "" {
		p.mu.Lock()
		defer p.mu.Unlock()
		return RemoveWorktree(ctx, p.RepoRoot, a.Cloud.TaskARN)
	}
	return nil
}

// waitForSocket polls for socketPath to exist and become connectable
// within timeout. We don't just stat — a stale file would race past
// stat — we try to dial.
func waitForSocket(socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("socket %s not ready after %s", socketPath, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// shutdownWorker POSTs /shutdown to the worker's unix socket. Best
// effort: the worker may already be dead.
func shutdownWorker(ctx context.Context, socketPath, token string) error {
	c := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", socketPath)
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/shutdown", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// killByPidFile reads pidPath and sends SIGTERM. Missing/garbage
// pidfile is not an error. Used as a fallback when the HTTP shutdown
// doesn't take effect.
func killByPidFile(pidPath string) error {
	body, err := os.ReadFile(pidPath)
	if err != nil {
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || pid <= 0 {
		return nil
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	_ = os.Remove(pidPath)
	return nil
}
