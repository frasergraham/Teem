package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/frasergraham/teem/internal/agent"
	"github.com/frasergraham/teem/internal/claudeflags"
	"github.com/frasergraham/teem/internal/notes"
	"github.com/frasergraham/teem/internal/provisioner"
	"github.com/frasergraham/teem/internal/state"
	"github.com/frasergraham/teem/internal/team"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage())
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]
	var err error
	switch sub {
	case "chat":
		err = runChat(args)
	case "init":
		err = runInit(args)
	case "audit":
		err = runAudit(args)
	case "start":
		err = runStart(args)
	case "stop":
		err = runStop(args)
	case "status":
		err = runStatus(args)
	case "pulse":
		err = runPulse(args)
	case "version":
		fmt.Println(versionString())
	case "-h", "--help", "help":
		fmt.Println(usage())
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n%s\n", sub, usage())
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() string {
	return strings.TrimSpace(`
teem — orchestrate Claude Code agents as a team

Usage:
  teem init                                            install plugin + show team or run the setup wizard
  teem start   [--foreground] [--listen :7777]         start the orchestrator daemon (headless by default)
  teem stop                                            stop the daemon
  teem status                                          report daemon state + registered teams
  teem chat    [--team teem.yaml] [--new-session]      register the current team with the daemon, launch Claude
  teem audit   [--agent ID] [--since RFC3339] [--limit 50] [--follow]
  teem pulse   <start|stop|pause|resume|tick|status> [--team t] [--interval 5m]
  teem version

Run 'teem <subcommand> -h' for flags.
`)
}

func versionString() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "dev"
}

// runChat: ensure the multi-tenant daemon is up, register the current
// team with it (lazy registration — idempotent), then exec Claude Code
// pointed at the daemon's per-team MCP URL.
func runChat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	teamPath := fs.String("team", "", "team YAML (default: ./teem.yaml or ./config/team.example.yaml)")
	useTailnet := fs.Bool("tailnet", true, "join the tailnet (used only when auto-starting the daemon)")
	listenAddr := fs.String("listen", ":7777", "daemon listen address (used only when auto-starting)")
	newSession := fs.Bool("new-session", false, "discard the saved leader session and start a fresh one")
	if err := fs.Parse(args); err != nil {
		return err
	}

	resolved, err := resolveTeamPath(*teamPath)
	if err != nil {
		return err
	}
	t, err := team.Load(resolved)
	if err != nil {
		return err
	}
	yamlBody, err := os.ReadFile(resolved)
	if err != nil {
		return fmt.Errorf("read %s: %w", resolved, err)
	}

	// 1. Ensure the (single, multi-tenant) daemon is running.
	if err := ensureDaemon(&daemonFlags{useTailnet: *useTailnet, listenAddr: *listenAddr}); err != nil {
		return err
	}

	// 2. Read the daemon's endpoint + shared bearer.
	ds, ok, err := readDaemonStateFile()
	if err != nil {
		return fmt.Errorf("read daemon state: %w", err)
	}
	if !ok {
		return errors.New("daemon state missing — try `teem start` manually")
	}

	// 3. Register this team with the daemon. Idempotent: re-registering
	//    an existing team returns the same URLs.
	repoRoot, _ := provisioner.ResolveRepoRoot("")
	regResp, err := registerWithDaemon(ds, string(yamlBody), repoRoot)
	if err != nil {
		return fmt.Errorf("register team: %w", err)
	}

	// 4. Write the MCP config Claude Code consumes.
	mcpCfgPath := filepath.Join(defaultStateDir(t.Name), "claude-mcp.json")
	if err := writeClaudeMCPConfig(mcpCfgPath, regResp.MCPURL); err != nil {
		return fmt.Errorf("write claude mcp config: %w", err)
	}

	// 5. Team brief + first-run plugin install + show any notes the
	//    leader left while we were away.
	brief := t.LeaderSystemPrompt()
	quietEnsurePlugin()
	showUnreadNotes(t.Name)

	// 6. Resolve or create the leader's persistent Claude Code session.
	//    First chat for a team creates the session id (--session-id);
	//    every subsequent chat resumes the same conversation
	//    (--resume <uuid>). Pulse will use the same id in phase 3.
	if *newSession {
		if err := os.Remove(leaderSessionPath(t.Name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clear leader session: %w", err)
		}
		fmt.Fprintf(os.Stderr, "[teem] --new-session: cleared saved leader session for %q\n", t.Name)
	}
	sessFlags, err := claudeSessionFlags(t.Name)
	if err != nil {
		return fmt.Errorf("leader session: %w", err)
	}

	// 7. Hand off to Claude Code.
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude CLI not found on PATH: %w (install Claude Code: https://docs.claude.com/en/docs/claude-code)", err)
	}
	argv := append([]string{"claude"}, sessFlags...)
	argv = append(argv,
		"--mcp-config", mcpCfgPath,
		"--append-system-prompt", brief,
	)
	argv = append(argv, claudeflags.ChannelFlags()...)
	fmt.Fprintf(os.Stderr, "[teem] team %q → %s — launching claude\n", t.Name, regResp.MCPURL)
	return syscall.Exec(claudePath, argv, os.Environ())
}

// claudeSessionFlags returns the claude CLI args needed to resume the
// team's persistent session, creating it on first invocation. Returns
// either [--resume <uuid>] or [--session-id <uuid>] depending on
// whether a session record already exists. The state file is written
// before the flag is returned so a successful first chat persists the
// id even if the user Ctrl-Cs out before claude finishes.
//
// Edge case: we may have saved a UUID but claude never actually
// materialised the conversation (first chat exited before claude
// flushed its session JSONL). In that case --resume fails with
// "No conversation found with session ID". Detect that by probing
// claude's session storage and fall back to --session-id with the same
// UUID so the next chat realises it for real.
func claudeSessionFlags(teamName string) ([]string, error) {
	sess, ok, err := loadLeaderSession(teamName)
	if err != nil {
		return nil, err
	}
	if ok {
		if claudeSessionExists(sess.SessionID) {
			return []string{"--resume", sess.SessionID}, nil
		}
		fmt.Fprintf(os.Stderr, "[teem] saved leader session %s not present in ~/.claude — recreating it under the same id\n", sess.SessionID)
		return []string{"--session-id", sess.SessionID}, nil
	}
	uuid, err := newSessionUUID()
	if err != nil {
		return nil, fmt.Errorf("generate session uuid: %w", err)
	}
	if err := saveLeaderSession(teamName, leaderSession{
		SessionID: uuid,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "[teem] new leader session %s for team %q\n", uuid, teamName)
	return []string{"--session-id", uuid}, nil
}

// claudeSessionExists checks whether Claude Code has a JSONL on disk
// for the given session id, scoped to the current working directory's
// project folder. Claude stores transcripts at
// ~/.claude/projects/<cwd-with-slashes-as-dashes>/<uuid>.jsonl.
// A missing file means --resume will fail; the caller should downgrade
// to --session-id with the same uuid so claude creates the conversation.
func claudeSessionExists(uuid string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	// Claude's path encoding: replace every `/` with `-`. Leading slash
	// becomes a leading dash. No other transformations.
	encoded := strings.ReplaceAll(cwd, "/", "-")
	path := filepath.Join(home, ".claude", "projects", encoded, uuid+".jsonl")
	_, err = os.Stat(path)
	return err == nil
}

// ensureDaemon starts the daemon detached if missing and waits for
// /healthz to answer.
func ensureDaemon(df *daemonFlags) error {
	if pid, alive := readDaemonPID(); alive {
		fmt.Fprintf(os.Stderr, "[teem] daemon already running (pid %d)\n", pid)
		return nil
	}
	if err := forkDetached(df); err != nil {
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, alive := readDaemonPID(); alive {
			st, ok, _ := readDaemonStateFile()
			if ok && st.Endpoint != "" && probeDaemonHealthz(st) {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not become ready in 15s — check %s", daemonLogPath())
}

// probeDaemonHealthz returns true if the daemon answers /healthz at the
// recorded endpoint. For tailnet endpoints the chat process can't dial
// the hostname without joining the tailnet itself; we trust the pid
// file in that case.
func probeDaemonHealthz(st daemonStateFile) bool {
	if !strings.HasPrefix(st.Endpoint, "http://127.0.0.1") && !strings.HasPrefix(st.Endpoint, "http://localhost") {
		return true
	}
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get(st.Endpoint + "/healthz")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == 200
}

// registerWithDaemon POSTs the team YAML to /control/teams. Re-registering
// an existing team returns the same URLs (200 OK rather than 201).
func registerWithDaemon(ds daemonStateFile, yamlBody, repoRoot string) (*registerResponse, error) {
	body, err := json.Marshal(map[string]string{
		"team_yaml": yamlBody,
		"repo_root": repoRoot,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, ds.Endpoint+"/control/teams", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+ds.Token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var r registerResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode register response: %w", err)
	}
	return &r, nil
}

// writeClaudeMCPConfig writes the JSON Claude Code's --mcp-config flag
// expects.
func writeClaudeMCPConfig(path, url string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(map[string]any{
		"mcpServers": map[string]any{
			"teem": map[string]any{
				"type": "http",
				"url":  url,
			},
		},
	}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, body)
}

// teamSearchPaths lists the file names `teem chat` searches for a team
// when --team is unset, in priority order. The wizard writes to the first
// entry.
var teamSearchPaths = []string{"teem.yaml", "config/team.example.yaml"}

// resolveTeamPath returns the team YAML to load. An explicit path is used
// as-is; otherwise the search chain is walked. Returns a helpful error
// pointing at `teem init` if nothing is found.
func resolveTeamPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	for _, p := range teamSearchPaths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no team YAML found (looked for %s). Run `teem init` to create one.", strings.Join(teamSearchPaths, ", "))
}

// newRootHandler composes the leader's HTTP routes. /mcp/* goes to the
// MCP server (no auth — its consumer is the leader's own Claude
// subprocess); /audit goes to the audit endpoints (bearer auth, since
// workers reach it across the tailnet).
func newRootHandler(mcp http.Handler, audit http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/mcp"):
			mcp.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/audit"):
			audit.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

// defaultStateDir returns the directory holding persistent-agent state
// files for the team. Lives alongside the audit log and worktrees so
// everything Teem persists is under ~/.teem.
func defaultStateDir(teamName string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".teem", "state")
	}
	return filepath.Join(home, ".teem", "state", slug(teamName))
}

// showUnreadNotes prints any leader-written notes accumulated since
// the user's last chat, then advances the read cursor so the same
// notes don't reappear. Failure is best-effort — we never abort the
// chat over a notes problem.
func showUnreadNotes(teamName string) {
	inbox, err := notes.Open(defaultNotesPath(teamName))
	if err != nil {
		return
	}
	defer inbox.Close()
	unread, err := inbox.Unread()
	if err != nil || len(unread) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\n[teem] %d note(s) from the leader since you were last here:\n", len(unread))
	for _, n := range unread {
		fmt.Fprintf(os.Stderr, "  • %s — %s\n", n.Timestamp.Local().Format("Jan 2 15:04"), n.Text)
	}
	fmt.Fprintln(os.Stderr, "")
	_ = inbox.MarkAllRead()
}

// defaultPlanPath returns the JSONL plan log path for a team. Lives
// under the team's state dir alongside the persistent-agent records.
func defaultPlanPath(teamName string) string {
	return filepath.Join(defaultStateDir(teamName), "plan.jsonl")
}

// defaultNotesPath returns the JSONL notes inbox path for a team.
// The user-facing "messages from the leader since you were away"
// channel.
func defaultNotesPath(teamName string) string {
	return filepath.Join(defaultStateDir(teamName), "notes.jsonl")
}

// defaultArchetypeSeqPath returns the JSON file produced by the
// pre-T9 per-role instance-id counter. The current allocator
// (internal/roster) reads this file once on first boot for
// migration, then ignores it.
func defaultArchetypeSeqPath(teamName string) string {
	return filepath.Join(defaultStateDir(teamName), "archetype-seq.json")
}

// defaultRosterPath returns the JSON file persisting the roster of
// worker names (wordlist allocations + reincarnation candidates).
// Replaces archetype-seq.json from T9 onward.
func defaultRosterPath(teamName string) string {
	return filepath.Join(defaultStateDir(teamName), "roster.json")
}

// defaultPulseRunningFlag returns the file path used to persist
// Pulse's "running" state. Presence at daemon startup means
// auto-resume.
func defaultPulseRunningFlag(teamName string) string {
	return filepath.Join(defaultStateDir(teamName), "pulse.running")
}

// defaultInFlightPath returns the JSONL log path the spawner uses to
// record start/end pairs for every job a worker is handed. Consulted
// on next daemon startup to emit job_interrupted for orphans.
func defaultInFlightPath(teamName string) string {
	return filepath.Join(defaultStateDir(teamName), "in-flight.jsonl")
}

// defaultSocketDir returns the directory under which per-agent unix
// sockets live for the subprocess local-worker model. Each socket
// path is socketDir/<agent-id>.sock with a sibling .pid file.
func defaultSocketDir(teamName string) string {
	return filepath.Join(defaultStateDir(teamName), "sockets")
}

// defaultMemoryDir returns the directory holding per-archetype memory
// markdown files for the team. One file per role: <dir>/<role>.md.
func defaultMemoryDir(teamName string) string {
	return filepath.Join(defaultStateDir(teamName), "memory")
}

// defaultRegistrationPath returns the file the daemon writes on each
// /control/teams registration so the team can be rebuilt after a
// restart. Holds the YAML the operator submitted plus repo metadata.
func defaultRegistrationPath(teamName string) string {
	return filepath.Join(defaultStateDir(teamName), "registration.json")
}

// drainTimeout returns the configured drain window for graceful
// daemon shutdown. Defaults to 30s. 0 disables drain. Read every time
// so it can be tuned without restarting.
func drainTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("TEEM_DRAIN_TIMEOUT"))
	if v == "" {
		return 30 * time.Second
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 30 * time.Second
	}
	if d < 0 {
		return 0
	}
	return d
}

// defaultAuditPath returns the on-disk audit log path for a team.
// Lives alongside the other ~/.teem state so it's predictable across
// sessions. Team name is slugged so YAML can't escape the path.
func defaultAuditPath(teamName string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".teem", "audit", "audit.jsonl")
	}
	return filepath.Join(home, ".teem", "audit", slug(teamName), "audit.jsonl")
}

// slug normalises an arbitrary name to a path-safe slug. Shared by
// defaultAuditPath and defaultWorktreeBase.
func slug(s string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		case r == ' ' || r == '_' || r == '-':
			return '-'
		}
		return -1
	}, s)
	out = strings.Trim(out, "-")
	if out == "" {
		return "default"
	}
	return out
}

// defaultWorktreeBase returns the directory under which local agent
// worktrees are placed for this team. Lives under ~/.teem alongside the
// tsnet state so it's predictable across sessions. The team name is
// slugged (lowercase, ascii-and-dash) so an arbitrary team.Name in the
// YAML can't escape into the path.
func defaultWorktreeBase(teamName string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".teem", "worktrees", slug(teamName))
}

// randomToken returns a 24-byte hex token used as the leader↔worker bearer
// when TEEM_WORKER_TOKEN isn't already set.
func randomToken() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failures only happen on very broken systems; falling
		// back to a fixed string would mask the failure, so panic.
		panic("teem: read random: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// cloudProvisionerFactory returns a factory the spawner uses to build
// provisioners for cloud backends. Today only Fargate; constructed lazily
// because it reads from env (AWS_REGION, TEEM_ECS_* etc) at build time.
func cloudProvisionerFactory(workerToken, leaderURL string, git provisioner.GitConfig, stateStore *state.Store) agent.CloudProvisionerFactory {
	return func(backend provisioner.Backend) (provisioner.Provisioner, error) {
		switch backend {
		case provisioner.BackendFargate:
			return provisioner.NewFargateProvisioner(workerToken, leaderURL, git, stateStore)
		default:
			return nil, fmt.Errorf("no cloud provisioner for backend %q", backend)
		}
	}
}

// branchPrefixOrDefault returns the operator-configured branch prefix
// or "teem/" — used only for the startup banner so the message matches
// what the worker daemon will actually do.
func branchPrefixOrDefault(s string) string {
	if s == "" {
		return "teem/"
	}
	return s
}

// readGitConfig pulls TEEM_GIT_* env into a GitConfig the provisioner
// can hand off to remote workers. Returns the same struct whether or not
// any vars are set; an empty RepoURL is the worker-side signal to skip
// cloning.
func readGitConfig() provisioner.GitConfig {
	return provisioner.GitConfig{
		RepoURL:      os.Getenv("TEEM_GIT_REPO_URL"),
		Token:        os.Getenv("TEEM_GIT_TOKEN"),
		Username:     os.Getenv("TEEM_GIT_USERNAME"),
		AuthorName:   os.Getenv("TEEM_GIT_AUTHOR_NAME"),
		AuthorEmail:  os.Getenv("TEEM_GIT_AUTHOR_EMAIL"),
		BranchPrefix: os.Getenv("TEEM_GIT_BRANCH_PREFIX"),
		AutoPush:     os.Getenv("TEEM_GIT_AUTO_PUSH"),
	}
}

func resolveAuthKey(envName string) string {
	if envName == "" {
		envName = "TS_AUTHKEY"
	}
	return os.Getenv(envName)
}

func normalizePort(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return addr
	}
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return ":" + port
	}
	return addr
}

