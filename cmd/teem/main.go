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
	"github.com/frasergraham/teem/internal/archmem"
	"github.com/frasergraham/teem/internal/claudeflags"
	"github.com/frasergraham/teem/internal/notes"
	"github.com/frasergraham/teem/internal/prompts"
	"github.com/frasergraham/teem/internal/provisioner"
	"github.com/frasergraham/teem/internal/state"
	"github.com/frasergraham/teem/internal/team"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usageString())
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
	case "agent":
		err = runAgent(args)
	case "prune-branches":
		err = runPruneBranches(args)
	case "usage":
		err = runUsageCmd(args)
	case "version":
		fmt.Println(versionString())
	case "-h", "--help", "help":
		fmt.Println(usageString())
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n%s\n", sub, usageString())
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usageString() string {
	return strings.TrimSpace(`
teem — orchestrate Claude Code agents as a team

Usage:
  teem init                                            install plugin + show team or run the setup wizard
  teem start   [--foreground] [--listen :7777]         start the orchestrator daemon (headless by default)
  teem stop                                            stop the daemon
  teem status                                          report daemon state + registered teams
  teem chat    [--team teem.yaml] [--no-remote-control] register the current team with the daemon, launch Claude
  teem audit   [--agent ID] [--since RFC3339] [--limit 50] [--follow]
  teem pulse   <start|stop|pause|resume|tick|status> [--team t] [--interval 5m]
  teem agent   <list|show|update> [<archetype>] [--prompt|--memory] [--team t]
  teem prune-branches [--team t] [--yes] [--force] [--retired-age 7d]
  teem usage   [--config ~/.teem/usage.yaml] [--state ~/.teem/state/usage.json]
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
	noRemoteControl := fs.Bool("no-remote-control", false, "do not pass --remote-control to claude (opt out of Remote Control for this chat)")
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
	// `teem chat` is the launch path that legitimately writes back —
	// mint+persist the team id into the operator's teem.yaml so the
	// daemon keys subsequent registrations off the same stable id
	// rather than freshly minting per chat. Best-effort: if the file
	// is read-only (rare), keep the in-memory id so this chat still
	// works; the daemon will mint into its own copy.
	if t.ID == "" {
		if id, werr := team.EnsureIDFile(resolved); werr == nil {
			t.ID = id
		} else {
			t.ID = team.NewID()
			fmt.Fprintf(os.Stderr, "[teem] could not write team id back to %s: %v (using %s for this run)\n", resolved, werr, t.ID)
		}
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
	mcpCfgPath := filepath.Join(defaultStateDir(t.ID), "claude-mcp.json")
	if err := writeClaudeMCPConfig(mcpCfgPath, regResp.MCPURL, t.ID, ds.Endpoint); err != nil {
		return fmt.Errorf("write claude mcp config: %w", err)
	}

	// 5. Team brief + first-run plugin install + show any notes the
	//    leader left while we were away. The base prompt comes from
	//    prompts.Builder.Leader() — team.LeaderSystemPrompt() folded
	//    with the operator override layer on disk. assembleLeaderBrief
	//    then prepends accumulated leader memory (per-team digest of
	//    prior sessions) so both compose into the final --append-
	//    system-prompt the leader subprocess receives at chat-start.
	pb := prompts.New(t, defaultPromptOverrideDir(t.ID))
	brief := assembleLeaderBrief(pb.Leader(), defaultMemoryDir(t.ID))
	quietEnsurePlugin()
	showUnreadNotes(t.ID)

	// 6. Mark that the operator has engaged with this team. Each
	//    `teem chat` is ephemeral (no --resume / --session-id) — the
	//    leader's persona rides entirely in the system prompt + leader
	//    memory + MCP tools. The on-disk leader-session record is kept
	//    purely as pulse's "operator has opted in" gate; the UUID is
	//    reused only for audit attribution, never for chat resumption.
	if err := ensureLeaderSessionMarker(t.ID); err != nil {
		return fmt.Errorf("leader session marker: %w", err)
	}

	// 7. Hand off to Claude Code.
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude CLI not found on PATH: %w (install Claude Code: https://docs.claude.com/en/docs/claude-code)", err)
	}
	argv := []string{"claude",
		"--mcp-config", mcpCfgPath,
		"--append-system-prompt", brief,
	}
	if !*noRemoteControl {
		// --remote-control [name]: optional name. Following token is
		// another --flag, so claude won't interpret it as the name.
		argv = append(argv, "--remote-control")
	}
	argv = append(argv, claudeflags.ChannelFlags()...)
	fmt.Fprintf(os.Stderr, "[teem] team %q → %s — launching claude\n", t.Name, regResp.MCPURL)
	return syscall.Exec(claudePath, argv, os.Environ())
}

// assembleLeaderBrief builds the system prompt the leader subprocess
// is launched with. base is the already-assembled prompt body (today
// prompts.Builder.Leader(): team.LeaderSystemPrompt + operator
// override layer). Loads any accumulated leader memory from memDir and
// prepends it as a "# Leader memory (prior sessions)" block above the
// base. A freshly-initialised file with header-only content (no digest
// text and no entries) is treated as empty so we don't inject a
// meaningless "Leader memory" section into the brief.
func assembleLeaderBrief(base, memDir string) string {
	store := archmem.New(memDir, nil)
	digest, entries, err := store.LoadParsed(archmem.LeaderRole)
	if err != nil {
		return base
	}
	if digest == "" && len(entries) == 0 {
		return base
	}
	body, err := store.Load(archmem.LeaderRole)
	if err != nil || strings.TrimSpace(body) == "" {
		return base
	}
	var b strings.Builder
	b.WriteString("# Leader memory (prior sessions)\n\n")
	b.WriteString(strings.TrimSpace(body))
	b.WriteString("\n\n---\n\n")
	b.WriteString(base)
	return b.String()
}

// ensureLeaderSessionMarker creates the per-team leader-session record
// if missing. The record's only remaining purpose is to gate Pulse:
// pulse stays quiet until the operator has run `teem chat` at least
// once. Each chat (and each pulse tick) is ephemeral — neither
// --resume nor --session-id is passed to claude — so the saved UUID
// is used only as an attribution token in audit events.
func ensureLeaderSessionMarker(teamID string) error {
	if _, ok, err := loadLeaderSession(teamID); err != nil {
		return err
	} else if ok {
		return nil
	}
	uuid, err := newSessionUUID()
	if err != nil {
		return fmt.Errorf("generate session uuid: %w", err)
	}
	if err := saveLeaderSession(teamID, leaderSession{
		SessionID: uuid,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[teem] new leader-session marker %s for team %q\n", uuid, teamID)
	return nil
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
// expects. Two servers are registered:
//
//   - "teem": the HTTP orchestrator MCP server (tools live here).
//   - "teem-channel": a stdio subprocess Claude Code spawns and
//     listens to for notifications/claude/channel. The shim
//     subscribes to the daemon's per-team SSE channel-events stream
//     and forwards each event over its stdio MCP transport. This is
//     the path that actually wakes the leader — the HTTP server's
//     PushChannel notifications go into the void because Claude Code
//     only fires channel listeners on stdio servers it spawned.
func writeClaudeMCPConfig(path, mcpURL, teamID, daemonEndpoint string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	shimPath, err := exec.LookPath("teem-channel")
	if err != nil {
		// Fall back to the bare name; claude will surface the error if
		// the binary isn't on PATH. The HTTP MCP entry is still
		// usable, so tools keep working.
		shimPath = "teem-channel"
	}
	body, err := json.MarshalIndent(map[string]any{
		"mcpServers": map[string]any{
			"teem": map[string]any{
				"type": "http",
				"url":  mcpURL,
			},
			"teem-channel": map[string]any{
				"type":    "stdio",
				"command": shimPath,
				"args":    []string{"--team", teamID, "--endpoint", daemonEndpoint},
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
// files for the team. Keyed by the team's stable id (t-<hex>) so renaming
// `team.name` in teem.yaml doesn't strand the on-disk state.
func defaultStateDir(teamID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".teem", "state")
	}
	return filepath.Join(home, ".teem", "state", teamID)
}

// showUnreadNotes prints any leader-written notes accumulated since
// the user's last chat, then advances the read cursor so the same
// notes don't reappear. Failure is best-effort — we never abort the
// chat over a notes problem.
func showUnreadNotes(teamID string) {
	inbox, err := notes.Open(defaultNotesPath(teamID))
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
func defaultPlanPath(teamID string) string {
	return filepath.Join(defaultStateDir(teamID), "plan.jsonl")
}

// defaultNotesPath returns the JSONL notes inbox path for a team.
// The user-facing "messages from the leader since you were away"
// channel.
func defaultNotesPath(teamID string) string {
	return filepath.Join(defaultStateDir(teamID), "notes.jsonl")
}

// defaultArchetypeSeqPath returns the JSON file produced by the
// pre-T9 per-role instance-id counter. The current allocator
// (internal/roster) reads this file once on first boot for
// migration, then ignores it.
func defaultArchetypeSeqPath(teamID string) string {
	return filepath.Join(defaultStateDir(teamID), "archetype-seq.json")
}

// defaultRosterPath returns the JSON file persisting the roster of
// worker names (wordlist allocations + reincarnation candidates).
// Replaces archetype-seq.json from T9 onward.
func defaultRosterPath(teamID string) string {
	return filepath.Join(defaultStateDir(teamID), "roster.json")
}

// defaultPulseRunningFlag returns the file path used to persist
// Pulse's "running" state. Presence at daemon startup means
// auto-resume.
func defaultPulseRunningFlag(teamID string) string {
	return filepath.Join(defaultStateDir(teamID), "pulse.running")
}

// defaultInFlightPath returns the JSONL log path the spawner uses to
// record start/end pairs for every job a worker is handed. Consulted
// on next daemon startup to emit job_interrupted for orphans.
func defaultInFlightPath(teamID string) string {
	return filepath.Join(defaultStateDir(teamID), "in-flight.jsonl")
}

// defaultSocketDir returns the directory under which per-agent unix
// sockets live for the subprocess local-worker model. Each socket
// path is socketDir/<agent-id>.sock with a sibling .pid file.
func defaultSocketDir(teamID string) string {
	return filepath.Join(defaultStateDir(teamID), "sockets")
}

// defaultMemoryDir returns the directory holding per-archetype memory
// markdown files for the team. One file per role: <dir>/<role>.md.
func defaultMemoryDir(teamID string) string {
	return filepath.Join(defaultStateDir(teamID), "memory")
}

// defaultPromptOverrideDir returns the directory holding operator-
// authored prompt-override files for the team. One file per role
// (including the synthetic "leader"): <dir>/<role>.md.
func defaultPromptOverrideDir(teamID string) string {
	return filepath.Join(defaultStateDir(teamID), "prompt-overrides")
}

// defaultRegistrationPath returns the file the daemon writes on each
// /control/teams registration so the team can be rebuilt after a
// restart. Holds the YAML the operator submitted plus repo metadata.
func defaultRegistrationPath(teamID string) string {
	return filepath.Join(defaultStateDir(teamID), "registration.json")
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
// Keyed by team_id so renames don't strand audit history.
func defaultAuditPath(teamID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".teem", "audit", "audit.jsonl")
	}
	return filepath.Join(home, ".teem", "audit", teamID, "audit.jsonl")
}

// defaultWorktreeBase returns the directory under which local agent
// worktrees are placed for this team. Keyed by team_id so renaming
// `team.name` doesn't strand existing worktrees.
func defaultWorktreeBase(teamID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".teem", "worktrees", teamID)
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
