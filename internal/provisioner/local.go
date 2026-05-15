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

	// ExitAfterIdle is the value passed to teem-worker as the
	// TEEM_EXIT_AFTER_IDLE env var. Ephemeral local workers self-
	// terminate after this idle window once they've handled at least
	// one job. Empty means "2s default"; "0" or "off" disables.
	ExitAfterIdle string

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
			Skill:       spec.Skill,
		}, nil
	}

	// Resolve the work directory: explicit YAML wins; otherwise an
	// isolated git worktree on a per-agent branch. Archetypes that
	// declare no_worktree: true skip the git path entirely — used by
	// roles whose work lives outside the codebase (e.g.
	// project_manager). They get the leader's repo root as cwd when
	// available, or os.TempDir() so the subprocess still has a real
	// directory to chdir into.
	workDir := spec.WorkingDir
	var branch string
	if workDir == "" && spec.NoWorktree {
		if p.RepoRoot != "" {
			workDir = p.RepoRoot
		} else {
			workDir = os.TempDir()
		}
	}
	if workDir == "" {
		if p.RepoRoot == "" || p.WorktreeBase == "" {
			return nil, ErrLocalNoWorkingDir
		}
		workDir = filepath.Join(p.WorktreeBase, spec.ID)
		branch = "teem/" + spec.ID
		// Pre-canonicalisation named workers wrote their worktree at
		// `<base>/<bare>/` on branch `teem/<bare>`. If the canonical
		// dir doesn't exist yet and the bare-name sibling does, this
		// is a still-orphaned pre-canonicalisation worker — rename
		// the worktree and branch in place so its commits aren't
		// stranded. Best-effort: any failure logs and falls through
		// to the normal EnsureWorktree path, which will then create a
		// fresh canonical worktree (operator can clean up the orphan
		// with `git worktree remove` + `git branch -D` if needed).
		p.adoptOrphanedWorktree(ctx, spec, workDir, branch)
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
			Skill:      spec.Skill,
		}
		if branch != "" {
			a.Cloud = &CloudPlacement{TaskARN: workDir}
			a.WorktreeBranch = branch
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
	// Ephemeral local workers self-exit after the configured idle
	// window once they've finished at least one job. Persistent
	// workers returned early above; everything reaching this point is
	// ephemeral.
	exit := p.ExitAfterIdle
	if exit == "" {
		exit = "2s"
	}
	env = append(env, "TEEM_EXIT_AFTER_IDLE="+exit)

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
		Skill:      spec.Skill,
	}
	if branch != "" {
		// Stash the worktree path on Cloud so Teardown knows what to
		// remove. CloudPlacement is named for the cloud case; we
		// reuse it rather than introduce another struct for a single
		// string.
		a.Cloud = &CloudPlacement{TaskARN: workDir}
		a.WorktreeBranch = branch
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
	// SIGTERM-by-pid if the HTTP shutdown doesn't take effect. When
	// the worker has already self-terminated (Stopped=true), skip
	// the live signals and only mop up on-disk artefacts.
	if a.SocketPath != "" {
		if !a.Stopped {
			_ = shutdownWorker(ctx, a.SocketPath, p.WorkerToken)
			_ = killByPidFile(strings.TrimSuffix(a.SocketPath, ".sock") + ".pid")
		} else {
			_ = os.Remove(strings.TrimSuffix(a.SocketPath, ".sock") + ".pid")
		}
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

// adoptOrphanedWorktree renames a pre-canonicalisation worker's
// worktree dir + branch onto the canonical id so the worker's prior
// commits don't get stranded. Fires only when:
//
//   - canonical workdir (`<base>/<role>-<bare>/`) doesn't exist yet,
//   - bare workdir (`<base>/<bare>/`) does exist,
//   - bare branch (`teem/<bare>`) exists.
//
// All three are pre-canonicalisation hallmarks; with them satisfied,
// it's safe to `git worktree move` + `git branch -m` to the canonical
// form. Best-effort throughout: any failure is logged and ignored so
// the normal EnsureWorktree path can still create a fresh canonical
// worktree.
func (p *LocalProvisioner) adoptOrphanedWorktree(ctx context.Context, spec AgentSpec, canonicalDir, canonicalBranch string) {
	if spec.Role == "" {
		return
	}
	bareName := strings.TrimPrefix(spec.ID, spec.Role+"-")
	if bareName == spec.ID {
		// spec.ID doesn't carry the `<role>-` prefix — nothing to
		// adopt; this isn't a canonicalised id.
		return
	}
	bareDir := filepath.Join(p.WorktreeBase, bareName)
	bareBranch := "teem/" + bareName
	if _, err := os.Stat(canonicalDir); err == nil {
		return // canonical already exists; nothing to adopt
	}
	if _, err := os.Stat(bareDir); err != nil {
		return // no bare orphan
	}
	if !branchExists(ctx, p.RepoRoot, bareBranch) {
		return // bare branch missing — can't safely move
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// git worktree move <bareDir> <canonicalDir> — moves both the
	// on-disk directory and git's bookkeeping in one shot.
	if err := runGit(ctx, p.RepoRoot, "worktree", "move", bareDir, canonicalDir); err != nil {
		fmt.Fprintf(os.Stderr, "[local-provisioner] adopt orphan worktree %s → %s: %v\n", bareDir, canonicalDir, err)
		return
	}
	if err := runGit(ctx, p.RepoRoot, "branch", "-m", bareBranch, canonicalBranch); err != nil {
		fmt.Fprintf(os.Stderr, "[local-provisioner] rename orphan branch %s → %s: %v\n", bareBranch, canonicalBranch, err)
	}
}

// CheckLiveness dials the worker's unix socket. A successful dial
// means the worker is still bound; ECONNREFUSED or "socket missing"
// means it has terminated. Other dial errors are reported as
// transient (the spawner keeps polling).
func (p *LocalProvisioner) CheckLiveness(ctx context.Context, a *Agent) error {
	if a == nil || a.SocketPath == "" {
		return nil
	}
	d := &net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "unix", a.SocketPath)
	if err == nil {
		_ = conn.Close()
		return nil
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
		return ErrAgentStopped
	}
	// Unwrap PathError / OpError for ENOENT — Go wraps the syscall
	// error inside net.OpError, which errors.Is sometimes misses.
	if oe, ok := err.(*net.OpError); ok && oe.Err != nil {
		if errors.Is(oe.Err, syscall.ECONNREFUSED) || errors.Is(oe.Err, syscall.ENOENT) {
			return ErrAgentStopped
		}
	}
	return err
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
