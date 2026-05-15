package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/frasergraham/teem/internal/agent"
	"github.com/frasergraham/teem/internal/archmem"
	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/channelbus"
	"github.com/frasergraham/teem/internal/inflight"
	"github.com/frasergraham/teem/internal/leaderstatus"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/messaging"
	"github.com/frasergraham/teem/internal/notes"
	"github.com/frasergraham/teem/internal/plan"
	"github.com/frasergraham/teem/internal/prompts"
	"github.com/frasergraham/teem/internal/pulse"
	"github.com/frasergraham/teem/internal/retention"
	"github.com/frasergraham/teem/internal/roster"
	"github.com/frasergraham/teem/internal/state"
	"github.com/frasergraham/teem/internal/tailnet"
	"github.com/frasergraham/teem/internal/team"
	"github.com/frasergraham/teem/internal/usage"
)

// leaderAwareRoles returns a RolesFunc whose result is the team's
// current archetype roles plus the reserved leader role. Used as a
// single source of truth so the archmem.Store validator and the
// Summarizer's Roles callback can't drift apart if the archetype set
// changes.
func leaderAwareRoles(t *team.Team) archmem.RolesFunc {
	return func() []string {
		archs := t.SnapshotArchetypes()
		roles := make([]string, 0, len(archs)+1)
		for _, a := range archs {
			roles = append(roles, a.Role)
		}
		roles = append(roles, archmem.LeaderRole)
		return roles
	}
}

// daemonFlags is the shared flag set for the daemon-mode commands.
//
// foreground keeps the daemon attached to the terminal (default is
// detached). detached is internal: the re-exec'd child sets it so the
// foreground branch runs without forking again.
type daemonFlags struct {
	useTailnet bool
	listenAddr string
	foreground bool
	detached   bool
}

func parseStartFlags(args []string) (*daemonFlags, error) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	df := &daemonFlags{}
	fs.BoolVar(&df.useTailnet, "tailnet", true, "join the tailnet via tsnet")
	fs.StringVar(&df.listenAddr, "listen", ":7777", "address the orchestrator listens on")
	fs.BoolVar(&df.foreground, "foreground", false, "stay attached to the terminal instead of detaching")
	fs.BoolVar(&df.detached, "detached", false, "internal: marks the re-exec'd child (do not pass)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return df, nil
}

// --- daemon entrypoints ---------------------------------------------------

// runStart launches the orchestrator daemon. Headless by default; pass
// --foreground to stay attached. Only one daemon per user at a time.
func runStart(args []string) error {
	df, err := parseStartFlags(args)
	if err != nil {
		return err
	}

	if pid, ok := readDaemonPID(); ok {
		return fmt.Errorf("daemon already running (pid %d). Run `teem stop` first.", pid)
	}

	if !df.foreground && !df.detached {
		return forkDetached(df)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveDaemon(ctx, df)
}

func runStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	pid, alive := readDaemonPID()
	if !alive {
		fmt.Fprintln(os.Stderr, "no running daemon")
		clearDaemonState()
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("kill %d: %w", pid, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, alive := readDaemonPID(); !alive {
			fmt.Printf("stopped daemon (pid %d)\n", pid)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("sent SIGTERM to pid %d (cleanup may still be in progress)\n", pid)
	return nil
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	pid, alive := readDaemonPID()
	if !alive {
		if pid != 0 {
			fmt.Printf("stopped (stale pid file: %d)\n", pid)
		} else {
			fmt.Println("stopped")
		}
		return nil
	}
	s, ok, _ := readDaemonStateFile()
	fmt.Printf("running  pid=%d\n", pid)
	if ok {
		fmt.Printf("  endpoint: %s\n", s.Endpoint)
		fmt.Printf("  started:  %s\n", s.StartedAt.Local().Format(time.RFC3339))
		if len(s.Teams) == 0 {
			fmt.Println("  teams:    (none registered yet)")
		} else {
			fmt.Printf("  teams:    %d\n", len(s.Teams))
			for _, name := range s.Teams {
				fmt.Printf("    - %s\n", name)
			}
		}
	}
	return nil
}

// --- daemon process: top-level state file ---------------------------------

// daemonStateFile is the on-disk endpoint discovery file at
// ~/.teem/daemon.json. teem chat / teem status read it.
type daemonStateFile struct {
	PID      int    `json:"pid"`
	Endpoint string `json:"endpoint"`     // http://<host>:<port>
	Token    string `json:"worker_token"` // shared bearer for /audit and /control
	// Teams holds display names for `teem status` output. The daemon
	// keys teams internally by team_id; this field is for humans only.
	Teams     []string  `json:"teams,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

func daemonHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".teem"
	}
	return filepath.Join(home, ".teem")
}

func daemonPIDPath() string  { return filepath.Join(daemonHomeDir(), "daemon.pid") }
func daemonJSONPath() string { return filepath.Join(daemonHomeDir(), "daemon.json") }
func daemonLogPath() string  { return filepath.Join(daemonHomeDir(), "daemon.log") }

func readDaemonPID() (int, bool) {
	body, err := os.ReadFile(daemonPIDPath())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || pid <= 0 {
		return pid, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return pid, false
	}
	return pid, true
}

func readDaemonStateFile() (daemonStateFile, bool, error) {
	body, err := os.ReadFile(daemonJSONPath())
	if err != nil {
		if os.IsNotExist(err) {
			return daemonStateFile{}, false, nil
		}
		return daemonStateFile{}, false, err
	}
	var s daemonStateFile
	if err := json.Unmarshal(body, &s); err != nil {
		return daemonStateFile{}, false, err
	}
	return s, true, nil
}

func writeDaemonStateFile(s daemonStateFile) error {
	if err := os.MkdirAll(daemonHomeDir(), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(daemonJSONPath(), body); err != nil {
		return err
	}
	return atomicWrite(daemonPIDPath(), []byte(strconv.Itoa(s.PID)+"\n"))
}

func clearDaemonState() {
	_ = os.Remove(daemonPIDPath())
	_ = os.Remove(daemonJSONPath())
	// The persistent worker_token file is intentionally NOT removed
	// here so the token survives `teem stop`/`teem start`.
}

// workerTokenPath returns the persistent token file location.
func workerTokenPath() string { return filepath.Join(daemonHomeDir(), "worker_token") }

// loadOrCreateWorkerToken reads the persistent worker token file or
// generates+writes a fresh one. The token is shared between the leader
// (this process) and every worker it spawns; stable across daemon
// restarts so workers don't all get 401'd after a bounce. To rotate,
// stop the daemon, remove ~/.teem/worker_token, then start again.
func loadOrCreateWorkerToken() string {
	if body, err := os.ReadFile(workerTokenPath()); err == nil {
		t := strings.TrimSpace(string(body))
		if t != "" {
			return t
		}
	}
	t := randomToken()
	if err := os.MkdirAll(daemonHomeDir(), 0o700); err == nil {
		_ = os.WriteFile(workerTokenPath(), []byte(t+"\n"), 0o600)
	}
	return t
}

func atomicWrite(path string, body []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- daemon process: serve the multi-tenant orchestrator ------------------

// daemon is the long-running state owned by serveDaemon. It holds a
// registry of teams; each registered team has its own MCP server,
// spawner, audit sink, and a worker→leader URL pointed at the daemon's
// per-team mount path.
type daemon struct {
	tnetNode   *tailnet.Node
	httpClient *http.Client
	endpoint   string          // public URL: http://host:port
	token      string          // shared bearer for /audit and /control
	baseCtx    context.Context // daemon lifetime — passed to per-team spawners

	// messagingNotifier is the daemon-global outbound push channel
	// (Telegram in v1) loaded from ~/.teem/messaging.yaml. nil when
	// messaging is disabled — combineHooks drops the nil hook silently.
	messagingNotifier messaging.Notifier
	messagingCfg      messaging.TelegramConfig
	messagingDedup    *messaging.Dedup

	// usageAgg is the daemon-global daily token-budget tracker.
	// Spawners consult it before provisioning new workers; the audit
	// hook chain feeds it KindUsageEvent rows so it stays current.
	// Nil-OK in tests that don't wire usage.
	usageAgg *usage.Aggregator

	// chatRunner is the subprocess seam the chat handler uses to spawn
	// the leader subprocess. Production wires it to the real
	// `claude -p` invocation; tests inject a fake. Nil ⇒ defaultChatRunner.
	chatRunner chatRunner

	// chatTimeout overrides the chat handler's per-request deadline.
	// Zero means 5 minutes (matches pulse.TickTimeout). Tests set this
	// to a small value so the timeout path is reachable in milliseconds.
	chatTimeout time.Duration

	mu    sync.Mutex
	teams map[string]*registeredTeam
}

type registeredTeam struct {
	team          *team.Team
	mcp           *mcpsrv.Server
	spawner       *agent.Spawner
	auditSink     *audit.FileSink
	auditH        http.Handler
	plan          *plan.Plan
	notes         *notes.Inbox
	pulse         *pulse.Pulse
	inFlight      *inflight.Log
	registry      *mcpsrv.Registry
	archMem       *archmem.Store
	archMemCancel context.CancelFunc
	leaderStatus  *leaderstatus.Store
	leaderURL     string
	registered    time.Time
	// transcriptsDir is the leader-side mirror root for worker
	// transcript files: <stateDir>/transcripts/<agent>/<job>.jsonl.
	transcriptsDir string
	// repoRoot is the git working tree the team's workers branch off
	// of. Empty for Fargate-only / repo-less teams; the dashboard's
	// "Active branches" section renders an empty placeholder in that
	// case. Comes straight from the registration payload.
	repoRoot string
	// channelBus fans channel events (PushChannel calls) out to every
	// teem-channel SSE subscriber. One bus per team; survives daemon
	// lifetime, no persistence.
	channelBus *channelbus.Bus

	// detectionMu guards channelsLive and serializes the per-team
	// channels-live ↔ fallback state machine. Held around BOTH the
	// channelbus Subscribe/cancel call and the flag mutation so the
	// "first subscriber" / "last subscriber" decision is TOCTOU-safe
	// even when two SSE handlers connect concurrently. See
	// docs/wake-strategy.md §5.
	detectionMu  sync.Mutex
	channelsLive bool
}

// serveDaemon runs the multi-tenant orchestrator until ctx is cancelled.
// Teams are registered lazily via POST /control/teams.
func serveDaemon(ctx context.Context, df *daemonFlags) error {
	hostname := os.Getenv("TEEM_DAEMON_HOSTNAME")
	if hostname == "" {
		hostname = "teem"
	}

	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: ctx}

	// Daemon-global messaging: optional ~/.teem/messaging.yaml drives the
	// outbound notifier wired into every team's audit hook chain. Missing
	// file = off (zero-config default). enabled=true + missing env var =
	// fail-fast so a misconfigured operator sees the error at startup
	// instead of discovering pings never arrive.
	if err := d.initMessaging(); err != nil {
		return err
	}

	// Usage-monitor wiring. Aggregator is daemon-global so the daily
	// budget applies across teams; KindUsageThrottle audit events are
	// fanned out to every team's audit sink so each leader sees its
	// local view of the throttle state.
	usageCfg, err := usage.LoadConfig(usage.DefaultConfigPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "[teemd] usage: bad config: %v (throttle disabled)\n", err)
		usageCfg = usage.Config{}
	}
	usageStore, err := usage.OpenStore(usage.DefaultStatePath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "[teemd] usage: state open failed: %v (throttle disabled)\n", err)
	} else {
		d.usageAgg = usage.NewAggregator(usageCfg, usageStore, d.onUsageThrottle)
		if usageCfg.DailyTokenBudget > 0 {
			fmt.Fprintf(os.Stderr, "[teemd] usage: cap=%d threshold=%.2f anchor=%q\n",
				usageCfg.DailyTokenBudget, usageCfg.EffectiveThreshold(), usageCfg.EffectiveAnchor())
		}
	}

	var listener net.Listener
	var mcpHost string
	if df.useTailnet {
		node, err := tailnet.New(tailnet.Config{
			Hostname: hostname,
			AuthKey:  resolveAuthKey("TS_AUTHKEY"),
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "[teemd] joining tailnet as %q...\n", hostname)
		if err := node.Start(ctx); err != nil {
			return err
		}
		defer node.Close()
		listener, err = node.Listen("tcp", df.listenAddr)
		if err != nil {
			return fmt.Errorf("tailnet listen: %w", err)
		}
		d.tnetNode = node
		d.httpClient = node.HTTPClient()
		mcpHost = hostname
	} else {
		var err error
		listener, err = net.Listen("tcp", "127.0.0.1"+normalizePort(df.listenAddr))
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		mcpHost = "127.0.0.1"
	}

	d.endpoint = fmt.Sprintf("http://%s%s", mcpHost, normalizePort(df.listenAddr))
	d.token = os.Getenv("TEEM_WORKER_TOKEN")
	if d.token == "" {
		// Persistent worker token: stored in its own file so it
		// survives across daemon stop/start cycles. Workers spawned
		// by an earlier daemon hold this token in their environment;
		// rotating it on every start orphans every in-flight worker
		// behind a 401 wall and renders the "workers survive a daemon
		// bounce" guarantee aspirational. Rotation is now opt-in via
		// `rm ~/.teem/worker_token`.
		d.token = loadOrCreateWorkerToken()
	}

	if err := writeDaemonStateFile(daemonStateFile{
		PID:       os.Getpid(),
		Endpoint:  d.endpoint,
		Token:     d.token,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("daemon state: %w", err)
	}
	defer clearDaemonState()

	fmt.Fprintf(os.Stderr, "[teemd] endpoint: %s\n", d.endpoint)

	// Restore every team that has a registration.json on disk. Done
	// before HTTP starts serving so the first inbound request sees a
	// fully-rehydrated daemon (pulses auto-resumed, workers
	// reattached). Best-effort: bad rows are logged and skipped.
	d.restoreTeams()

	fmt.Fprintf(os.Stderr, "[teemd] ready. Stop with `teem stop` or kill %d\n", os.Getpid())

	httpSrv := &http.Server{
		Handler:           d.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Retention GC: spawns only when an operator has explicitly
	// configured one of the TTL knobs. Default is "never delete"; the
	// daemon preserves all stopped-agent and transcript history
	// indefinitely unless opted in.
	retCfg := retention.LoadConfig()
	if retCfg.Enabled() {
		fmt.Fprintf(os.Stderr, "[teemd] retention: stopped_agent_ttl=%s transcript_ttl=%s sweep=%s\n",
			retCfg.StoppedAgentTTL, retCfg.TranscriptTTL, retentionSweepInterval(retCfg))
		safeGo("retention.gc", func() { d.runRetentionGC(retCfg) })
	}

	// Cross-project peer awareness (XP1): once per interval, write a
	// "what your peers are doing" digest into each leader's archmem
	// memory file. Enabled by default; TEEM_PEERAWARE_INTERVAL=0
	// disables. With a single team registered the loop becomes a no-op.
	if peerInterval := peerAwareConfig(); peerInterval > 0 {
		fmt.Fprintf(os.Stderr, "[teemd] peeraware: interval=%s\n", peerInterval)
		safeGo("peeraware.loop", func() { d.runPeerAware(peerInterval) })
	}

	// Branch cleanup: sweep dead teem/* branches every 12h by default.
	// Set TEEM_PRUNE_INTERVAL=0 to disable. Default is on with logging
	// so operators don't accumulate hundreds of stale branches.
	if pruneInterval := pruneSweepConfig(); pruneInterval > 0 {
		fmt.Fprintf(os.Stderr, "[teemd] prune-branches: interval=%s\n", pruneInterval)
		safeGo("prune.sweep", func() { d.runPruneSweep(pruneInterval) })
	}

	serverErr := make(chan error, 1)
	go func() {
		err := httpSrv.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		} else {
			serverErr <- nil
		}
	}()
	defer func() {
		// 1. Stop accepting new HTTP requests.
		ctx2, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx2)

		// 2. Graceful drain — wait up to TEEM_DRAIN_TIMEOUT for
		//    in-flight worker jobs to finish. After this expires,
		//    anything still running gets killed by Spawner.Stop's
		//    context cancellation. The startup reconcile of the next
		//    daemon will emit job_interrupted for it.
		if drain := drainTimeout(); drain > 0 {
			fmt.Fprintf(os.Stderr, "[teemd] draining in-flight jobs (up to %s)...\n", drain)
			drainCtx, dcancel := context.WithTimeout(context.Background(), drain)
			d.mu.Lock()
			teams := make([]*registeredTeam, 0, len(d.teams))
			for _, rt := range d.teams {
				teams = append(teams, rt)
			}
			d.mu.Unlock()
			for _, rt := range teams {
				if err := rt.spawner.Drain(drainCtx); err != nil {
					fmt.Fprintf(os.Stderr, "[teemd] %s: drain timed out with %d job(s) still in flight\n", rt.team.Name, rt.spawner.TotalInFlight())
				}
			}
			dcancel()
		}

		// 3. Final teardown.
		d.mu.Lock()
		for _, rt := range d.teams {
			// Daemon shutdown: preserve the pulse running-flag so
			// `teem start` auto-resumes Pulse on the next boot. Operator
			// opt-out goes through `teem pulse stop` (the flag-clearing
			// Stop variant), not through bouncing the daemon.
			rt.pulse.StopForShutdown()
			rt.spawner.Stop()
			_ = rt.auditSink.Close()
			_ = rt.plan.Close()
			_ = rt.notes.Close()
			if rt.archMemCancel != nil {
				rt.archMemCancel()
			}
			if rt.inFlight != nil {
				_ = rt.inFlight.Close()
			}
		}
		d.mu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-serverErr:
		return err
	}
}

// handler is the daemon's top-level HTTP dispatcher.
//
//	/healthz                        liveness
//	/control/teams         (POST)   register a team
//	/control/teams         (GET)    list registered teams
//	/control/teams/<name>  (DELETE) unregister a team
//	/teams/<name>/mcp/*             that team's MCP server
//	/teams/<name>/audit             that team's audit endpoint
//
// /control and /teams routes require bearer auth via d.token.
func (d *daemon) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/healthz":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
		case path == "/control/teams":
			d.requireAuth(w, r, d.handleControlTeamsCollection)
		case strings.HasPrefix(path, "/control/teams/") && strings.HasSuffix(path, "/ping"):
			// Dashboard "Ping leader" button. Unauth on purpose: shares
			// the dashboard's localhost-only security model (no
			// per-user auth yet). When the daemon binds to tailnet
			// rather than 127.0.0.1, anyone on the tailnet can hit
			// this — same boundary as the dashboard itself.
			d.handlePingTeam(w, r)
		case strings.HasPrefix(path, "/control/teams/") && isDashboardPulseAction(path):
			// Dashboard "Pulse" panel form posts (start/stop/config
			// sub-paths). Same unauth rationale as /ping — the
			// dashboard is the localhost / tailnet UI surface and
			// can't carry the bearer token. The GET on /pulse stays
			// auth'd; only the action sub-paths are exempt.
			d.handleControlTeamsItem(w, r)
		case strings.HasPrefix(path, "/control/teams/") && strings.HasSuffix(path, "/chat"):
			// Dashboard chat panel. Same tailnet-boundary auth model as
			// /ping; spawns a one-shot leader `claude -p` and streams
			// the response as SSE.
			d.handleChatTeam(w, r)
		case strings.HasPrefix(path, "/control/teams/"):
			d.requireAuth(w, r, d.handleControlTeamsItem)
		case strings.HasPrefix(path, "/teams/"):
			d.handleTeamRoute(w, r)
		case path == "/" || path == "/ui" || path == "/ui/":
			// Dashboard. Unauth on purpose: tailnet is the security
			// boundary (same model as the MCP endpoint). Read-only
			// for now — no actions exposed.
			d.renderDashboard(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

// isDashboardPulseAction reports whether the request path matches one
// of the dashboard's pulse-panel form-post sub-paths
// (/control/teams/<id>/pulse/{start,stop,config}). Those endpoints are
// served unauth so the dashboard's HTML form can hit them; the bare
// /pulse GET stays auth'd.
func isDashboardPulseAction(path string) bool {
	for _, suffix := range []string{"/pulse/start", "/pulse/stop", "/pulse/config"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

func (d *daemon) requireAuth(w http.ResponseWriter, r *http.Request, h func(http.ResponseWriter, *http.Request)) {
	if r.Header.Get("Authorization") != "Bearer "+d.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h(w, r)
}

// --- /control/teams handlers ----------------------------------------------

type registerRequest struct {
	TeamYAML     string `json:"team_yaml"`
	RepoRoot     string `json:"repo_root,omitempty"`
	WorktreeBase string `json:"worktree_base,omitempty"`
}

// teamRegistration is the on-disk snapshot the daemon uses to rebuild
// a team after a restart. Lives at ~/.teem/state/<team-id>/registration.json.
type teamRegistration struct {
	TeamYAML     string    `json:"team_yaml"`
	RepoRoot     string    `json:"repo_root,omitempty"`
	WorktreeBase string    `json:"worktree_base,omitempty"`
	RegisteredAt time.Time `json:"registered_at"`
}

func writeTeamRegistration(teamID string, reg teamRegistration) error {
	path := defaultRegistrationPath(teamID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

func removeTeamRegistration(teamID string) {
	_ = os.Remove(defaultRegistrationPath(teamID))
}

// migrateLegacyTeamDirs is the pre-T33 → T33 migration: walk every
// per-team state dir under ~/.teem/state and, when it doesn't already
// look like a `t-<hex>` id directory, mint an id and rename the state
// / audit / worktree dirs to use it. Idempotent: a re-run skips any
// dir already in the canonical form.
//
// The mint also writes the new id back into the registration.json
// TeamYAML body so the next daemon load picks it up cleanly, and
// (best-effort) into the operator's teem.yaml at repo_root if it
// exists and is writable. A failure on the operator's yaml is logged
// and the migration continues — the in-memory id still works for the
// current run.
func migrateLegacyTeamDirs(home string) {
	migrateLegacyTeamDirsIn(filepath.Join(home, ".teem"))
}

// migrateLegacyTeamDirsIn is the testable form of migrateLegacyTeamDirs:
// it walks state/audit/worktrees under an explicit base dir. The home
// shim above just calls this with `<home>/.teem`.
//
// Partial-failure recovery: audit and worktrees rename first
// (best-effort, log on failure but continue). State renames last and
// is the canonical marker — if `state/<id>` exists, the team is
// considered migrated. A failed audit/worktree rename strands those
// dirs under the legacy slug, but the consumer paths (defaultAuditPath,
// defaultWorktreeBase) are keyed by ID; the strand just means audit
// history / worktrees aren't visible at the new id. We log a warning
// rather than crash, so the daemon still boots; a re-run of the
// migration will see `state/<id>` already canonical and skip.
func migrateLegacyTeamDirsIn(base string) {
	stateRoot := filepath.Join(base, "state")
	auditRoot := filepath.Join(base, "audit")
	worktreesRoot := filepath.Join(base, "worktrees")

	entries, err := os.ReadDir(stateRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		oldName := e.Name()
		if team.IsCanonicalID(oldName) {
			continue // already migrated
		}
		regPath := filepath.Join(stateRoot, oldName, "registration.json")
		body, err := os.ReadFile(regPath)
		if err != nil {
			// State dir without a registration — likely orphaned. Skip.
			continue
		}
		var reg teamRegistration
		if err := json.Unmarshal(body, &reg); err != nil {
			fmt.Fprintf(os.Stderr, "[teemd] migration: skip %s (bad registration.json: %v)\n", oldName, err)
			continue
		}

		// Mint via EnsureIDFile against a temp copy so we can both
		// (a) get the id, and (b) capture the rewritten YAML body to
		// re-persist into registration.json. EnsureIDFile reuses an
		// existing id in the YAML if present, so a yaml that already
		// has `id:` doesn't get a fresh one.
		tmpFile, err := writeTempYAML(reg.TeamYAML)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[teemd] migration: skip %s (temp yaml: %v)\n", oldName, err)
			continue
		}
		newID, err := team.EnsureIDFile(tmpFile)
		if err != nil {
			_ = os.Remove(tmpFile)
			fmt.Fprintf(os.Stderr, "[teemd] migration: skip %s (mint id: %v)\n", oldName, err)
			continue
		}
		updated, _ := os.ReadFile(tmpFile)
		_ = os.Remove(tmpFile)
		reg.TeamYAML = string(updated)

		newStateDir := filepath.Join(stateRoot, newID)
		if _, err := os.Stat(newStateDir); err == nil {
			fmt.Fprintf(os.Stderr, "[teemd] migration: skip %s (target %s already exists)\n", oldName, newID)
			continue
		}

		// Rename audit + worktrees FIRST (each best-effort: a missing
		// source is fine; a failed rename logs a warning and continues
		// rather than aborting — state's rename is the canonical
		// migration marker). This ordering means a crash between the
		// first two and the state rename leaves both legacy and the
		// in-progress state dir intact, so a re-run can complete.
		oldAudit := filepath.Join(auditRoot, oldName)
		newAudit := filepath.Join(auditRoot, newID)
		if _, err := os.Stat(oldAudit); err == nil {
			if rerr := os.Rename(oldAudit, newAudit); rerr != nil {
				fmt.Fprintf(os.Stderr, "[teemd] migration: rename audit %s -> %s: %v (stranded under legacy slug; not fatal)\n", oldName, newID, rerr)
			} else {
				fmt.Fprintf(os.Stderr, "[teemd] migrated audit dir: %s -> %s\n", oldAudit, newAudit)
			}
		}

		oldWT := filepath.Join(worktreesRoot, oldName)
		newWT := filepath.Join(worktreesRoot, newID)
		if _, err := os.Stat(oldWT); err == nil {
			if rerr := os.Rename(oldWT, newWT); rerr != nil {
				fmt.Fprintf(os.Stderr, "[teemd] migration: rename worktrees %s -> %s: %v (stranded under legacy slug; not fatal)\n", oldName, newID, rerr)
			} else {
				fmt.Fprintf(os.Stderr, "[teemd] migrated worktree dir: %s -> %s\n", oldWT, newWT)
			}
		}

		// State LAST: this is the canonical marker — if this rename
		// succeeds, the team is considered migrated and a re-run skips
		// it. If it fails, audit/worktrees are still under the legacy
		// slug AND state is too, so a future re-run starts fresh.
		if err := os.Rename(filepath.Join(stateRoot, oldName), newStateDir); err != nil {
			fmt.Fprintf(os.Stderr, "[teemd] migration: rename state %s -> %s: %v (migration aborted for this team)\n", oldName, newID, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "[teemd] migrated team to id %s: %s -> %s\n", newID, filepath.Join(stateRoot, oldName), newStateDir)

		// Write the id-bearing YAML back into the new registration.json.
		if werr := writeTeamRegistration(newID, reg); werr != nil {
			fmt.Fprintf(os.Stderr, "[teemd] migration: write new registration.json for %s: %v\n", newID, werr)
		}

		// Best-effort: also back-fill the SAME minted id into the
		// operator's teem.yaml at repo_root so the next `teem chat`
		// from that working tree reuses the migrated state instead of
		// minting a fresh id and stranding it.
		if reg.RepoRoot != "" {
			candidate := filepath.Join(reg.RepoRoot, "teem.yaml")
			if _, err := os.Stat(candidate); err == nil {
				if werr := team.SetIDFile(candidate, newID); werr != nil {
					fmt.Fprintf(os.Stderr, "[teemd] migration: could not back-fill %s: %v (id-only state dir migrated)\n", candidate, werr)
				}
			}
		}
	}
}

// restoreTeams rebuilds every team that has a registration.json on
// disk. Best-effort: a corrupt file or a YAML that no longer parses
// logs and continues — we'd rather serve N-1 teams than refuse to
// start. Called once at daemon boot, before serving HTTP.
//
// Runs the pre-T33 slug→id migration first so the rest of this
// function only sees id-keyed directories.
func (d *daemon) restoreTeams() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	migrateLegacyTeamDirs(home)
	stateRoot := filepath.Join(home, ".teem", "state")
	for _, c := range planTeamRestores(stateRoot) {
		// Append the synthesised project_manager archetype if the
		// team is wired to a tracker. Best-effort: if the role
		// already exists in the YAML (operator added it manually)
		// AddArchetype returns ErrArchetypeExists and we skip.
		if pm := team.MaybePMArchetype(c.team); pm != nil {
			if err := c.team.AddArchetype(*pm); err != nil && !errors.Is(err, team.ErrArchetypeExists) {
				fmt.Fprintf(os.Stderr, "[teemd] %s: append project_manager: %v\n", c.team.Name, err)
			}
		}
		rt, err := d.buildTeamServices(c.team, c.reg.RepoRoot, c.reg.WorktreeBase)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[teemd] skip %s: build services: %v\n", c.dirName, err)
			continue
		}
		// Preserve the original registration time so the dashboard
		// doesn't show "registered just now" after every restart.
		if !c.reg.RegisteredAt.IsZero() {
			rt.registered = c.reg.RegisteredAt
		}
		d.mu.Lock()
		d.teams[c.team.ID] = rt
		d.mu.Unlock()
		fmt.Fprintf(os.Stderr, "[teemd] restored team %q (id %s, pulse %s)\n", c.team.Name, c.team.ID, pulseStateLabel(rt))
		// Reconcile workers and persistent agents asynchronously so a
		// slow Fargate API call doesn't block boot.
		rtRef := rt
		safeGo("reconcile.restored:"+rtRef.team.ID, func() {
			if n := rtRef.spawner.ReconcileLocalSockets(context.Background()); n > 0 {
				fmt.Fprintf(os.Stderr, "[teemd] %s: reattached %d local worker(s)\n", rtRef.team.Name, n)
			}
			if n := rtRef.spawner.Reconcile(context.Background()); n > 0 {
				fmt.Fprintf(os.Stderr, "[teemd] %s: reconciled %d persistent agent(s)\n", rtRef.team.Name, n)
			}
		})
	}
	d.persistStateSnapshot()
}

// restoreCandidate is one fully-loaded team ready to wire into d.teams.
// Returned by planTeamRestores after dedup so callers can register
// without re-checking for collisions.
type restoreCandidate struct {
	dirName string // basename of the state dir the candidate came from
	team    *team.Team
	reg     teamRegistration
}

// planTeamRestores scans state dirs under stateRoot, parses each
// registration.json, and returns one candidate per restored team. It
// guarantees idempotency in two dimensions:
//
//  1. Same id from multiple state dirs (typical when a legacy slug dir
//     survived migration because its target id-dir already existed):
//     the dir whose registration.json was modified most recently wins;
//     the others are logged and skipped.
//  2. Distinct ids for the same Name (typical phantom — a past
//     partial migration minted a fresh id while the operator's
//     canonical state dir already existed): prefer the candidate whose
//     id is the one referenced by `<reg.RepoRoot>/teem.yaml` (the
//     operator's source of truth — same signal migrateLegacyTeamDirs
//     uses when back-filling); fall back to most-recent-mtime only if
//     no candidate is teem.yaml-anchored (or both are). A loud WARN
//     names both ids so the operator can remove the stale dir.
//
// Side-effects despite the "plan" framing: when a registration's YAML
// lacks an id, this function mints one via EnsureIDFile and writes the
// back-filled YAML into the registration.json (mirroring the
// pre-refactor inline mint path in restoreTeams). The mint is
// idempotent — once written, a second call observes the id and skips
// the mint, so running daemon boot twice in a row leaves disk state
// unchanged.
func planTeamRestores(stateRoot string) []restoreCandidate {
	entries, err := os.ReadDir(stateRoot)
	if err != nil {
		return nil
	}
	var all []loadedTeam
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		regPath := filepath.Join(stateRoot, e.Name(), "registration.json")
		info, err := os.Stat(regPath)
		if err != nil {
			continue
		}
		body, err := os.ReadFile(regPath)
		if err != nil {
			continue
		}
		var reg teamRegistration
		if err := json.Unmarshal(body, &reg); err != nil {
			fmt.Fprintf(os.Stderr, "[teemd] skip %s: bad registration.json: %v\n", e.Name(), err)
			continue
		}
		tmpFile, err := writeTempYAML(reg.TeamYAML)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[teemd] skip %s: temp yaml: %v\n", e.Name(), err)
			continue
		}
		t, err := team.Load(tmpFile)
		if err != nil {
			_ = os.Remove(tmpFile)
			fmt.Fprintf(os.Stderr, "[teemd] skip %s: invalid yaml: %v\n", e.Name(), err)
			continue
		}
		// Load is pure-read since the team-id refactor: mint+persist
		// here so restored teams without an id (legacy registrations
		// that escaped migrateLegacyTeamDirs) still get one written
		// back into the saved YAML body.
		if t.ID == "" {
			id, werr := team.EnsureIDFile(tmpFile)
			if werr != nil {
				_ = os.Remove(tmpFile)
				fmt.Fprintf(os.Stderr, "[teemd] skip %s: mint id: %v\n", e.Name(), werr)
				continue
			}
			t.ID = id
		}
		if updated, rerr := os.ReadFile(tmpFile); rerr == nil && string(updated) != reg.TeamYAML {
			reg.TeamYAML = string(updated)
			_ = writeTeamRegistration(t.ID, reg)
		}
		_ = os.Remove(tmpFile)
		all = append(all, loadedTeam{
			dirName:  e.Name(),
			regMTime: info.ModTime(),
			reg:      reg,
			t:        t,
		})
	}
	// Pass 1: collapse same-id duplicates. The teem.yaml anchor is
	// irrelevant here (both candidates share an id by definition) so
	// pickWinner falls through to mtime — most recent reg.json mtime
	// wins, ties broken by lexicographic dirName.
	byID := make(map[string]loadedTeam, len(all))
	for _, l := range all {
		prev, dup := byID[l.t.ID]
		if !dup {
			byID[l.t.ID] = l
			continue
		}
		winner, loser := pickWinner(prev, l)
		fmt.Fprintf(os.Stderr, "[teemd] skip %s: id %s already restored from %s (idempotent dedup; consider removing the stale state dir under ~/.teem/state/)\n",
			loser.dirName, loser.t.ID, winner.dirName)
		byID[l.t.ID] = winner
	}
	// Pass 2: collapse same-name distinct-id collisions (the phantom
	// case). Iterate byID in sorted key order so log output is
	// deterministic when ≥3 candidates share a Name. WARN names both
	// ids so the operator can investigate.
	idKeys := make([]string, 0, len(byID))
	for k := range byID {
		idKeys = append(idKeys, k)
	}
	sort.Strings(idKeys)
	byName := make(map[string]loadedTeam, len(byID))
	for _, k := range idKeys {
		l := byID[k]
		prev, dup := byName[l.t.Name]
		if !dup {
			byName[l.t.Name] = l
			continue
		}
		winner, loser := pickWinner(prev, l)
		fmt.Fprintf(os.Stderr, "[teemd] WARN: team %q has two state dirs with different ids — keeping %s (from %s); skipping %s (from %s). Operator should remove the stale dir under ~/.teem/state/.\n",
			l.t.Name, winner.t.ID, winner.dirName, loser.t.ID, loser.dirName)
		byName[l.t.Name] = winner
	}
	out := make([]restoreCandidate, 0, len(byName))
	for _, l := range byName {
		out = append(out, restoreCandidate{
			dirName: l.dirName,
			team:    l.t,
			reg:     l.reg,
		})
	}
	// Stable order for callers/tests: by id.
	sort.Slice(out, func(i, j int) bool { return out[i].team.ID < out[j].team.ID })
	return out
}

// loadedTeam is one parsed state-dir entry mid-flight inside
// planTeamRestores. Hoisted to package scope only so pickWinner can
// reference it; not part of any external contract.
type loadedTeam struct {
	dirName  string
	regMTime time.Time
	reg      teamRegistration
	t        *team.Team
}

// pickWinner returns (winner, loser) for two candidates that collide on
// id (pass 1) or Name (pass 2). The teem.yaml anchor at
// `<reg.RepoRoot>/teem.yaml` is the operator's source of truth — the
// id it carries was either chosen by the operator or back-filled by
// migrateLegacyTeamDirs — so if exactly one candidate is anchored
// there, it wins regardless of mtime. This is the round-2 fix for the
// phantom case where the phantom dir was created *after* the canonical
// dir and so had a newer mtime under the round-1 pure-mtime rule.
//
// When neither (or both) candidate(s) are anchored, fall through to the
// mtime rule: most recent reg.json mtime wins; ties broken by
// lexicographic dirName for deterministic output.
func pickWinner(a, b loadedTeam) (winner, loser loadedTeam) {
	aAnchored := candidateAnchored(a)
	bAnchored := candidateAnchored(b)
	if aAnchored && !bAnchored {
		return a, b
	}
	if bAnchored && !aAnchored {
		return b, a
	}
	if a.regMTime.After(b.regMTime) {
		return a, b
	}
	if b.regMTime.After(a.regMTime) {
		return b, a
	}
	if a.dirName < b.dirName {
		return a, b
	}
	return b, a
}

// candidateAnchored reports whether the candidate's id is the one
// recorded in `<reg.RepoRoot>/teem.yaml`. Empty RepoRoot, missing /
// unreadable file, or unparseable YAML all read as "not anchored" —
// callers fall back to the mtime tiebreaker.
func candidateAnchored(l loadedTeam) bool {
	if l.reg.RepoRoot == "" {
		return false
	}
	yamlPath := filepath.Join(l.reg.RepoRoot, "teem.yaml")
	t, err := team.Load(yamlPath)
	if err != nil {
		return false
	}
	return t.ID != "" && t.ID == l.t.ID
}

func pulseStateLabel(rt *registeredTeam) string {
	if rt.pulse == nil {
		return "—"
	}
	if rt.pulse.Running() {
		if rt.pulse.Paused() {
			return "paused"
		}
		return "running"
	}
	return "off"
}

type registerResponse struct {
	Team     string `json:"team"`
	MCPURL   string `json:"mcp_url"`
	AuditURL string `json:"audit_url"`
}

func (d *daemon) handleControlTeamsCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		d.handleRegister(w, r)
	case http.MethodGet:
		d.handleListTeams(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (d *daemon) handleControlTeamsItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/control/teams/")
	if rest == "" {
		http.Error(w, "bad team id", http.StatusBadRequest)
		return
	}
	// Split into <key> (id or name alias) and optional <subresource>.
	key := rest
	sub := ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		key, sub = rest[:i], rest[i+1:]
	}
	rt := d.resolveTeam(key)
	if rt == nil {
		http.NotFound(w, r)
		return
	}
	// Subsequent map mutations / state-file paths must use the
	// canonical id, not the URL key (which may be a slug alias).
	id := rt.team.ID

	switch {
	case sub == "":
		// Whole-team operations.
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		d.mu.Lock()
		delete(d.teams, id)
		d.mu.Unlock()
		rt.pulse.Stop()
		rt.spawner.Stop()
		_ = rt.auditSink.Close()
		_ = rt.plan.Close()
		_ = rt.notes.Close()
		if rt.archMemCancel != nil {
			rt.archMemCancel()
		}
		if rt.inFlight != nil {
			_ = rt.inFlight.Close()
		}
		removeTeamRegistration(id)
		d.persistStateSnapshot()
		w.WriteHeader(http.StatusNoContent)
	case sub == "pulse":
		d.handlePulseControl(w, r, rt, "")
	case strings.HasPrefix(sub, "pulse/"):
		d.handlePulseControl(w, r, rt, strings.TrimPrefix(sub, "pulse/"))
	case strings.HasPrefix(sub, "tasks/"):
		d.handleControlTaskAction(w, r, rt, strings.TrimPrefix(sub, "tasks/"))
	default:
		http.NotFound(w, r)
	}
}

// pulseStatus is the GET response shape and the start-result shape.
// WakePrompt carries the active first-turn message; UseDefaultWakePrompt
// is true when the operator hasn't supplied an override (useful as a
// dashboard hint to render the textarea as a placeholder rather than a
// pre-filled value).
type pulseStatus struct {
	Running              bool      `json:"running"`
	Paused               bool      `json:"paused"`
	Interval             string    `json:"interval"`
	LastTick             time.Time `json:"last_tick,omitempty"`
	TickCount            int64     `json:"tick_count"`
	WakePrompt           string    `json:"wake_prompt"`
	UseDefaultWakePrompt bool      `json:"use_default_wake_prompt"`
	DefaultWakePrompt    string    `json:"default_wake_prompt"`
}

// pulseCommand is the POST body for action-style requests. WakePrompt
// is *string so callers can distinguish "leave alone" (nil) from
// "clear override / fall back to default" (empty string).
type pulseCommand struct {
	Action     string  `json:"action"`   // start|stop|pause|resume|tick|config (action also derived from URL sub-path)
	Interval   string  `json:"interval"` // for start/config; Go duration string
	Reason     string  `json:"reason"`   // for pause
	WakePrompt *string `json:"wake_prompt,omitempty"`
}

// handlePulseControl handles GET/POST under /control/teams/<id>/pulse,
// including the sub-paths /pulse/start, /pulse/stop, /pulse/config used
// by the dashboard's pulse panel. subAction (when non-empty) overrides
// any cmd.Action in the body so form-style POSTs don't need to repeat
// the verb.
func (d *daemon) handlePulseControl(w http.ResponseWriter, r *http.Request, rt *registeredTeam, subAction string) {
	switch r.Method {
	case http.MethodGet:
		if subAction != "" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, currentPulseStatus(rt))
	case http.MethodPost:
		cmd, err := decodePulseCommand(r)
		if err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		action := cmd.Action
		if subAction != "" {
			action = subAction
		}
		switch action {
		case "start":
			if cmd.Interval != "" {
				dur, err := time.ParseDuration(cmd.Interval)
				if err != nil {
					http.Error(w, "bad interval: "+err.Error(), http.StatusBadRequest)
					return
				}
				rt.pulse.SetInterval(dur)
			}
			if cmd.WakePrompt != nil {
				if err := rt.pulse.SetWakePrompt(*cmd.WakePrompt); err != nil {
					http.Error(w, "set wake prompt: "+err.Error(), http.StatusInternalServerError)
					return
				}
			}
			rt.pulse.Start(d.baseCtx)
		case "stop":
			rt.pulse.Stop()
		case "pause":
			if err := rt.pulse.Pause(cmd.Reason); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		case "resume":
			if err := rt.pulse.Resume(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		case "tick":
			// Synchronous one-off tick — useful for `teem pulse tick`
			// and for testing. Runs on a background context so the
			// HTTP request returning doesn't cancel the tick.
			safeGo("pulse.tick:"+rt.team.Name, func() { _ = rt.pulse.Tick(d.baseCtx, "manual") })
		case "config":
			var dur time.Duration
			if cmd.Interval != "" {
				parsed, err := time.ParseDuration(cmd.Interval)
				if err != nil {
					http.Error(w, "bad interval: "+err.Error(), http.StatusBadRequest)
					return
				}
				dur = parsed
			}
			if err := rt.pulse.UpdateConfig(dur, cmd.WakePrompt); err != nil {
				http.Error(w, "update config: "+err.Error(), http.StatusInternalServerError)
				return
			}
		default:
			http.Error(w, "unknown action: "+action, http.StatusBadRequest)
			return
		}
		// Dashboard form posts come with Accept: text/html — redirect
		// back to the team page so the operator stays in context.
		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.Redirect(w, r, "/teams/"+rt.team.ID, http.StatusSeeOther)
			return
		}
		writeJSON(w, http.StatusOK, currentPulseStatus(rt))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// decodePulseCommand reads a pulseCommand from either an
// application/x-www-form-urlencoded body (dashboard form posts) or a
// JSON body (CLI / programmatic callers). Empty bodies decode to a
// zero-value command so callers can still drive the URL sub-action.
func decodePulseCommand(r *http.Request) (pulseCommand, error) {
	var cmd pulseCommand
	ctype := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ctype, ';'); i >= 0 {
		ctype = ctype[:i]
	}
	ctype = strings.TrimSpace(ctype)
	switch ctype {
	case "application/x-www-form-urlencoded", "multipart/form-data":
		if err := r.ParseForm(); err != nil {
			return cmd, err
		}
		cmd.Action = r.FormValue("action")
		cmd.Reason = r.FormValue("reason")
		// Dashboard splits interval into number + unit; either form
		// works here. A bare `interval` value wins if both are sent.
		if v := r.FormValue("interval"); v != "" {
			cmd.Interval = v
		} else if num := r.FormValue("interval_value"); num != "" {
			unit := r.FormValue("interval_unit")
			if unit == "" {
				unit = "m"
			}
			cmd.Interval = num + unit
		}
		// Form-post wake-prompt: the textarea is always present in
		// the form, even when blank — that's how the operator clears
		// an override. Detect "submitted as part of the form" via the
		// PostForm map so a missing field doesn't accidentally clear.
		if r.PostForm.Has("wake_prompt") {
			v := r.PostForm.Get("wake_prompt")
			cmd.WakePrompt = &v
		}
	default:
		if r.Body == nil {
			return cmd, nil
		}
		if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil && err != io.EOF {
			return cmd, err
		}
	}
	return cmd, nil
}

// handlePingTeam serves POST /control/teams/<id>/ping — the dashboard's
// manual "Ping leader" button. Fires one pulse tick (trigger=manual)
// when the team isn't paused and no tick is in flight, otherwise
// returns a status that the dashboard turns into a flash message.
//
// Auth: localhost-only dashboard, no per-user auth yet. Operator
// identity isn't carried; the audit event uses agent_id="operator" as
// a placeholder until we have real auth.
func (d *daemon) handlePingTeam(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Path is /control/teams/<id>/ping.
	rest := strings.TrimPrefix(r.URL.Path, "/control/teams/")
	key := strings.TrimSuffix(rest, "/ping")
	if key == "" || strings.ContainsRune(key, '/') {
		http.Error(w, "bad team id", http.StatusBadRequest)
		return
	}
	rt := d.resolveTeam(key)
	if rt == nil {
		http.NotFound(w, r)
		return
	}
	// Audit event + ping_ts redirect always carry the canonical id so
	// the team page (also id-keyed) can correlate the tick.
	id := rt.team.ID

	if rt.pulse == nil {
		http.Error(w, "pulse not configured", http.StatusInternalServerError)
		return
	}
	// channels-live = operator chat is active and already driving the
	// leader. Two writers on the same session file is a concurrent-
	// write hazard, and the ping is redundant. Read under detectionMu
	// so we serialize against the SSE handler's flag flips.
	rt.detectionMu.Lock()
	live := rt.channelsLive
	rt.detectionMu.Unlock()
	if live {
		d.pingRespond(w, r, id, http.StatusConflict, "ping_skipped_chat_active",
			"operator chat session is active — leader is already awake via channels; ping is unnecessary", 0)
		return
	}
	if rt.pulse.Paused() {
		d.pingRespond(w, r, id, http.StatusConflict, "paused",
			"pulse paused; `teem pulse resume` first", 0)
		return
	}
	if rt.pulse.Busy() {
		d.pingRespond(w, r, id, http.StatusAccepted, "busy",
			"tick already in progress", 0)
		return
	}

	if rt.auditSink != nil {
		_ = rt.auditSink.Write(audit.Event{
			Timestamp: time.Now().UTC(),
			AgentID:   "operator",
			Kind:      audit.Kind("pulse_tick"),
			Message:   "manual ping from dashboard",
			Meta:      map[string]any{"trigger": "manual"},
		})
	}
	// Capture the timestamp we redirect with so the team page can scan
	// the audit log for the matching leader pulse_tick and render the
	// actual outcome (success / failure / still-working) instead of a
	// fire-and-forget banner.
	pingedAt := time.Now().Unix()
	safeGo("pulse.ping:"+rt.team.ID, func() { _ = rt.pulse.Tick(d.baseCtx, "manual") })
	d.pingRespond(w, r, id, http.StatusOK, "pinged", "ping queued", pingedAt)
}

// pingRespond emits the right shape based on the request's Accept
// header: a redirect with ?flash=<tag> for form posts (so the dashboard
// surfaces a flash), or a plain text body for curl / fetch callers.
// pingTS (if non-zero) is appended as &ping_ts=<unix> so the team page
// can correlate the redirect with the leader's pulse_tick audit event.
func (d *daemon) pingRespond(w http.ResponseWriter, r *http.Request, teamID string, code int, flash, body string, pingTS int64) {
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		loc := "/teams/" + teamID + "?flash=" + flash
		if pingTS > 0 {
			loc += "&ping_ts=" + strconv.FormatInt(pingTS, 10)
		}
		http.Redirect(w, r, loc, http.StatusSeeOther)
		return
	}
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

func currentPulseStatus(rt *registeredTeam) pulseStatus {
	wp := rt.pulse.WakePrompt()
	custom := rt.pulse.IsCustomWakePrompt()
	return pulseStatus{
		Running:              rt.pulse.Running(),
		Paused:               rt.pulse.Paused(),
		Interval:             rt.pulse.Interval().String(),
		LastTick:             rt.pulse.LastTick(),
		TickCount:            rt.pulse.TickCount(),
		WakePrompt:           wp,
		UseDefaultWakePrompt: !custom,
		DefaultWakePrompt:    pulse.DefaultWakePrompt(),
	}
}

func (d *daemon) handleListTeams(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]map[string]any, 0, len(d.teams))
	for id, rt := range d.teams {
		out = append(out, map[string]any{
			"id":         id,
			"name":       rt.team.Name,
			"mcp_url":    d.endpoint + "/teams/" + id + "/mcp",
			"audit_url":  d.endpoint + "/teams/" + id + "/audit",
			"registered": rt.registered.Format(time.RFC3339),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (d *daemon) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.TeamYAML == "" {
		http.Error(w, "team_yaml is required", http.StatusBadRequest)
		return
	}
	// Parse the YAML by writing to a temp file and using team.Load
	// (pure-read since the team-id refactor). When the submitted YAML
	// lacks an `id:`, EnsureIDFile mints one into the temp file; we
	// re-read so the id-bearing YAML is what gets persisted into
	// registration.json.
	tmpFile, err := writeTempYAML(req.TeamYAML)
	if err != nil {
		http.Error(w, fmt.Sprintf("write yaml: %v", err), http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmpFile)
	t, err := team.Load(tmpFile)
	if err != nil {
		http.Error(w, fmt.Sprintf("validate team: %v", err), http.StatusBadRequest)
		return
	}
	if t.ID == "" {
		id, werr := team.EnsureIDFile(tmpFile)
		if werr != nil {
			http.Error(w, fmt.Sprintf("mint team id: %v", werr), http.StatusInternalServerError)
			return
		}
		t.ID = id
	}
	if updated, rerr := os.ReadFile(tmpFile); rerr == nil {
		req.TeamYAML = string(updated)
	}

	// Idempotent: re-registering an existing team is a no-op that
	// returns the same URLs. (Trade-off: we don't pick up YAML edits
	// without an explicit DELETE first. Document.)
	d.mu.Lock()
	existing, ok := d.teams[t.ID]
	d.mu.Unlock()
	if ok {
		writeJSON(w, http.StatusOK, registerResponse{
			Team:     existing.team.Name,
			MCPURL:   d.endpoint + "/teams/" + t.ID + "/mcp",
			AuditURL: d.endpoint + "/teams/" + t.ID + "/audit",
		})
		return
	}

	// Append the synthesised project_manager archetype if the team
	// is wired to a tracker. See restoreTeams for the symmetric
	// call; both paths must wire it so registrations and daemon
	// restarts present the same roster.
	if pm := team.MaybePMArchetype(t); pm != nil {
		if err := t.AddArchetype(*pm); err != nil && !errors.Is(err, team.ErrArchetypeExists) {
			fmt.Fprintf(os.Stderr, "[teemd] %s: append project_manager: %v\n", t.Name, err)
		}
	}
	rt, err := d.buildTeamServices(t, req.RepoRoot, req.WorktreeBase)
	if err != nil {
		http.Error(w, fmt.Sprintf("build team services: %v", err), http.StatusInternalServerError)
		return
	}
	d.mu.Lock()
	d.teams[t.ID] = rt
	d.mu.Unlock()
	if err := writeTeamRegistration(t.ID, teamRegistration{
		TeamYAML:     req.TeamYAML,
		RepoRoot:     req.RepoRoot,
		WorktreeBase: req.WorktreeBase,
		RegisteredAt: rt.registered,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "[teemd] warning: persist registration for %q: %v\n", t.Name, err)
	}
	d.persistStateSnapshot()

	// Best-effort reconcile in two passes:
	//
	// 1. Local subprocess workers from the previous daemon run. Their
	//    sockets are still on disk; probe each, register live ones,
	//    sweep stale.
	// 2. Persistent agents from the team YAML (tailnet-hosted; either
	//    operator-managed local or Fargate).
	safeGo("reconcile.registered:"+t.ID, func() {
		if n := rt.spawner.ReconcileLocalSockets(context.Background()); n > 0 {
			fmt.Fprintf(os.Stderr, "[teemd] reattached %d local worker(s) for %s\n", n, t.Name)
		}
		if n := rt.spawner.Reconcile(context.Background()); n > 0 {
			fmt.Fprintf(os.Stderr, "[teemd] reconciled %d persistent agent(s) for %s\n", n, t.Name)
		}
	})

	writeJSON(w, http.StatusCreated, registerResponse{
		Team:     t.Name,
		MCPURL:   rt.leaderURL + "/mcp",
		AuditURL: rt.leaderURL + "/audit",
	})
}

// buildTeamServices stands up the per-team MCP server, spawner, and
// audit sink. Repo root and worktree base come from the chat client's
// CWD (so worktrees land where the operator expects).
func (d *daemon) buildTeamServices(t *team.Team, repoRoot, worktreeBase string) (*registeredTeam, error) {
	if t.ID == "" {
		// Defensive: every code path that constructs a Team should
		// have minted an id by now. Mint one if not so we never key a
		// per-team filesystem path on the empty string.
		t.ID = team.NewID()
		fmt.Fprintf(os.Stderr, "[teemd] warning: team %q had no id; minted %s\n", t.Name, t.ID)
	}
	if worktreeBase == "" {
		worktreeBase = defaultWorktreeBase(t.ID)
	}
	leaderURL := d.endpoint + "/teams/" + t.ID
	stateStore := state.NewStore(defaultStateDir(t.ID))
	gitCfg := readGitConfig()

	auditPath := defaultAuditPath(t.ID)
	auditSink, err := audit.OpenFile(auditPath)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}

	planPath := defaultPlanPath(t.ID)
	planStore, err := plan.Open(planPath)
	if err != nil {
		_ = auditSink.Close()
		return nil, fmt.Errorf("plan: %w", err)
	}

	notesInbox, err := notes.Open(defaultNotesPath(t.ID))
	if err != nil {
		_ = auditSink.Close()
		_ = planStore.Close()
		return nil, fmt.Errorf("notes: %w", err)
	}

	leaderStatusStore, err := leaderstatus.Open(defaultLeaderStatusPath(t.ID))
	if err != nil {
		_ = auditSink.Close()
		_ = planStore.Close()
		_ = notesInbox.Close()
		return nil, fmt.Errorf("leader_status: %w", err)
	}

	// In-flight log for durability. Opened before reconcile so the
	// next steps can both (a) emit job_interrupted for orphans and
	// (b) hand it to the spawner for future jobs.
	inFlightLog, err := inflight.Open(defaultInFlightPath(t.ID))
	if err != nil {
		_ = auditSink.Close()
		_ = planStore.Close()
		_ = notesInbox.Close()
		return nil, fmt.Errorf("inflight: %w", err)
	}
	// Reconcile: any "start" without a matching "end" in the log was
	// interrupted by the previous shutdown. Emit a final audit event
	// so the leader can see what's incomplete, then truncate so we
	// don't re-report on the next restart.
	if orphans, err := inFlightLog.Outstanding(); err == nil && len(orphans) > 0 {
		for _, o := range orphans {
			_ = auditSink.Write(audit.Event{
				Timestamp: time.Now().UTC(),
				AgentID:   o.AgentID,
				JobID:     o.JobID,
				Kind:      audit.KindJobInterrupted,
				Message:   "daemon shutdown interrupted this job",
				Meta: map[string]any{
					"prompt_preview": o.PromptPreview,
					"started_at":     o.StartedAt.Format(time.RFC3339),
				},
			})
		}
		fmt.Fprintf(os.Stderr, "[teemd] %s: marked %d job(s) interrupted by prior shutdown\n", t.Name, len(orphans))
		_ = inFlightLog.Reset()
	}

	bs := bus.NewMemBus()
	reg := mcpsrv.NewRegistry()
	chBus := channelbus.New(0)

	// Archetype memory store: per-team directory of per-role markdown
	// files the leader injects as baseline context for every freshly
	// spawned worker. Created up-front so the spawner can read from
	// it and the audit hook can append to it.
	archMemDir := defaultMemoryDir(t.ID)
	archMemStore := archmem.New(archMemDir, leaderAwareRoles(t))
	archMemStore.SweepTmp()

	transcriptsDir := filepath.Join(defaultStateDir(t.ID), "transcripts")

	// Prompt builder: layered assembly of the leader's and each
	// archetype's system prompt with an operator-override layer on
	// disk. Shared by the CLI, the MCP read_prompt/append_prompt
	// tools, and the spawner's per-worker bake-in.
	promptBuilder := prompts.New(t, defaultPromptOverrideDir(t.ID))

	// Roster: per-team worker-name allocator. On first open after the
	// T9 rollout (no existing roster.json), migrate legacy
	// `<role>-N` ids from the previous archetype-seq.json counter
	// and any historical transcripts subdirs so they participate in
	// reincarnation. The legacy file is left in place — we no longer
	// read it, but keeping it makes a downgrade non-destructive.
	rosterPath := defaultRosterPath(t.ID)
	rost, err := roster.Open(rosterPath)
	if err != nil {
		_ = auditSink.Close()
		_ = planStore.Close()
		_ = notesInbox.Close()
		return nil, fmt.Errorf("roster: %w", err)
	}
	roleList := func() []string {
		archs := t.SnapshotArchetypes()
		roles := make([]string, 0, len(archs))
		for _, a := range archs {
			roles = append(roles, a.Role)
		}
		return roles
	}()
	if n := rost.MigrateLegacy(defaultArchetypeSeqPath(t.ID), transcriptsDir, roleList, nil); n > 0 {
		fmt.Fprintf(os.Stderr, "[teemd] %s: migrated %d legacy worker id(s) into the roster\n", t.Name, n)
	}

	spawner := agent.NewSpawner(d.baseCtx, t, bs, reg, agent.Config{
		HTTPClient:          d.httpClient,
		WorkerToken:         d.token,
		CloudProvisioner:    cloudProvisionerFactory(d.token, leaderURL, gitCfg, stateStore),
		RepoRoot:            repoRoot,
		WorktreeBase:        worktreeBase,
		LeaderURL:           leaderURL,
		StateStore:          stateStore,
		AuditSink:           auditSink,
		Roster:              rost,
		InFlight:            inFlightLog,
		SocketDir:           defaultSocketDir(t.ID),
		LoadArchetypeMemory: archMemStore.Load,
		// Spawner.LoadArchetypePrompt is (role) -> string; Builder.Archetype
		// now signals "role not declared" via the bool. The spawner only
		// reaches here for roles it just resolved from the team YAML, so
		// an empty string on miss is a safe degenerate case.
		LoadArchetypePrompt: func(role string) string {
			s, _ := promptBuilder.Archetype(role)
			return s
		},
		UsageQuota: d.spawnerQuota(),
	})

	srv, err := mcpsrv.New(mcpsrv.Config{
		Bus:            bs,
		Team:           t,
		Registry:       reg,
		Spawner:        spawner,
		Audit:          auditSink,
		Plan:           planStore,
		Notes:          notesInbox,
		TranscriptsDir: transcriptsDir,
		ArchMem:        archMemStore,
		LeaderStatus:   leaderStatusStore,
		Prompts:        promptBuilder,
		ChannelSink: func(content string, meta map[string]string) {
			chBus.Publish(channelbus.Event{Content: content, Meta: meta})
		},
	})
	if err != nil {
		_ = auditSink.Close()
		_ = planStore.Close()
		_ = notesInbox.Close()
		return nil, err
	}

	// Pulse: autonomous-leader heartbeat. Built per team, NOT started
	// (phase 4's `teem pulse start` activates it). Needs the team's
	// MCP URL via a small JSON file pulse hands to claude.
	pulseMCPPath := filepath.Join(defaultStateDir(t.ID), "pulse-mcp.json")
	shimPath, _ := exec.LookPath("teem-channel")
	if err := pulse.WriteMCPConfig(pulseMCPPath, leaderURL+"/mcp", t.ID, d.endpoint, shimPath); err != nil {
		_ = auditSink.Close()
		_ = planStore.Close()
		_ = notesInbox.Close()
		return nil, fmt.Errorf("pulse mcp config: %w", err)
	}
	pulseInst := pulse.New(pulse.Config{
		TeamName: t.Name,
		TeamID:   t.ID,
		LoadSession: func() (string, bool, error) {
			s, ok, err := loadLeaderSession(t.ID)
			if err != nil || !ok {
				return "", false, err
			}
			return s.SessionID, true, nil
		},
		PauseFile:      filepath.Join(defaultStateDir(t.ID), "pulse.paused"),
		RunningFile:    defaultPulseRunningFlag(t.ID),
		WakePromptFile: filepath.Join(defaultStateDir(t.ID), "pulse-wake.txt"),
		MCPConfig:      pulseMCPPath,
		RepoRoot:       repoRoot,
		Plan:           planStore,
		Audit:          auditSink,
		Registry:       reg,
		OnUsage:        d.usageRecorder(),
	})
	// Auto-resume Pulse if it was running before the daemon
	// restarted. Operator opt-out is `teem pulse stop` (which clears
	// the flag) or `teem pulse pause` (which leaves it alone but
	// skips ticks).
	if pulseInst.WasRunning() {
		pulseInst.Start(d.baseCtx)
		fmt.Fprintf(os.Stderr, "[teemd] auto-resumed Pulse for %q\n", t.Name)
	}

	// Wake hook: publish on the in-process leader.wake bus topic
	// whenever a worker emits a job-terminal event. Today no chat
	// client consumes it because runChat exec()s directly into claude;
	// kept as an additive T6 signal future chat clients can subscribe
	// to.
	wakeHook := func(events []audit.Event) {
		for _, e := range events {
			if !isWakeKind(e.Kind) {
				continue
			}
			payload, _ := json.Marshal(map[string]string{
				"kind":     string(e.Kind),
				"agent_id": e.AgentID,
				"job_id":   e.JobID,
			})
			_ = bs.Publish(d.baseCtx, bus.Message{
				ID:        bus.NewID(),
				Topic:     "leader.wake",
				From:      e.AgentID,
				Kind:      bus.KindStatus,
				Payload:   payload,
				CreatedAt: time.Now().UTC(),
			})
		}
	}

	// Stop hook: when a worker emits worker_stopped, reconcile the
	// spawner's bookkeeping (registry → stopped, teardown skipping
	// /shutdown, drop subscriptions). Runs in a goroutine so the
	// audit POST returns promptly; HandleWorkerStopped is idempotent
	// against duplicates.
	stopHook := func(events []audit.Event) {
		for _, e := range events {
			if e.Kind != audit.KindWorkerStopped {
				continue
			}
			agentID := e.AgentID
			go spawner.HandleWorkerStopped(context.Background(), agentID)
		}
	}

	// Archmem hook: on every job-terminal event, append a one-line
	// summary to the archetype's per-role memory file. The role is
	// resolved from the registry; if the agent is gone we skip
	// silently (audit fallback would need to scan history and isn't
	// worth it for an append).
	archMemHook := func(events []audit.Event) {
		for _, e := range events {
			if e.Kind != audit.KindJobComplete && e.Kind != audit.KindJobError {
				continue
			}
			role := lookupRole(reg, e.AgentID)
			if role == "" {
				continue
			}
			status := "done"
			summary, _ := e.Meta["output"].(string)
			if e.Kind == audit.KindJobError {
				status = "error"
				if summary == "" {
					summary = e.Message
				}
			}
			entry := archmem.Entry{
				Timestamp: e.Timestamp,
				AgentID:   e.AgentID,
				JobID:     e.JobID,
				Status:    status,
				Summary:   shortSummary(summary),
			}
			if err := archMemStore.AppendEntry(role, entry); err != nil && !errors.Is(err, archmem.ErrUnknownRole) {
				fmt.Fprintf(os.Stderr, "[archmem] append %q: %v\n", role, err)
			}
		}
	}

	// Channel hook: push a one-line summary of selected audit events
	// into the leader's claude session via the team's MCP server
	// (Claude Code "channels"). Fire-and-forget; safe when no leader is
	// currently subscribed. The filter intentionally excludes
	// high-volume kinds like heartbeats and pulse_tick echoes.
	channelHook := makeChannelHook(srv.PushChannel)

	// Messaging hook: out-of-band push to the operator's phone (Telegram
	// in v1) for the narrow operator-must-see set — awaiting_approval,
	// blockers, severity=question decisions, leader errors. Daemon-global
	// notifier, per-team formatter (needs plan for task titles). nil when
	// messaging.yaml is absent / disabled / missing token; combineHooks
	// drops the nil hook silently.
	var messagingHook auditHook
	if d.messagingNotifier != nil && d.messagingDedup != nil {
		fmtr := messaging.MessageFormatter{
			TeamID:           t.ID,
			DashboardBaseURL: d.messagingCfg.DashboardBaseURL,
			TaskTitle:        messaging.FromPlan(planStore),
		}
		messagingHook = makeMessagingHook(d.messagingNotifier, fmtr, d.messagingDedup)
	}

	// Pulse audit-nudge hook: schedules a debounced pulse Tick on
	// interesting worker events when channels are NOT live (the L2
	// fallback path in docs/wake-strategy.md). Pulse internally gates
	// on its channels-live flag — when chat is connected, this hook is
	// a no-op and the channel block does the waking. Safe whether or
	// not Pulse has been Started: NudgeFromAudit early-returns when
	// the loop isn't running.
	pulseNudgeHook := func(events []audit.Event) {
		pulseInst.NudgeFromAudit(events)
	}

	// Summarizer goroutine: rolling digest + retention pruning per
	// role. Best-effort — failures log to stderr and the next tick
	// retries. Uses the operator's Claude Code auth via `claude -p`
	// subprocess; if the binary isn't on PATH we still run the loop
	// so retention pruning happens, just without an LLM digest.
	archMemCtx, archMemCancel := context.WithCancel(d.baseCtx)
	var completer archmem.Completer
	if path, err := exec.LookPath("claude"); err == nil {
		completer = archmem.NewClaudeSubprocessCompleter(path, repoRoot)
	} else {
		fmt.Fprintf(os.Stderr, "[archmem] claude CLI not on PATH; digest will be skipped: %v\n", err)
	}
	summarizer := &archmem.Summarizer{
		Store:    archMemStore,
		Complete: completer,
		Roles:    leaderAwareRoles(t),
	}
	safeGo("archmem.summarizer:"+t.ID, func() { _ = summarizer.Run(archMemCtx) })

	// Scheduled project-manager tick. Only fires for tracker-configured
	// teams (the PM archetype was synthesised by MaybePMArchetype
	// earlier in the same code path); a zero/negative PollInterval
	// disables the loop while leaving the on-demand leader spawn alive.
	if t.Tracker != nil && t.Tracker.Type != "" {
		interval := t.Tracker.PollInterval
		if interval == 0 {
			interval = pmLoopDefaultInterval
		}
		if interval > 0 {
			fmt.Fprintf(os.Stderr, "[teemd] %s: pm-loop interval=%s\n", t.Name, interval)
			pmCfg := PMLoopConfig{
				TeamName: t.Name,
				Interval: interval,
				Spawner:  spawner,
				Audit:    auditSink,
			}
			safeGo("pm.loop:"+t.ID, func() { pmCfg.Loop(d.baseCtx) })
		}
	}

	return &registeredTeam{
		team:      t,
		mcp:       srv,
		spawner:   spawner,
		auditSink: auditSink,
		// Audit handler fans every POST out to: write to disk, bump
		// the agent's LastSeen, publish on bus topic "leader.wake" for
		// terminal worker events, reconcile worker_stopped, append to
		// archetype memory, push channel notifications to any connected
		// leader chat, and nudge pulse's debounced audit-nudge tick
		// (suppressed by pulse when channels are live; active when
		// chat is disconnected — see docs/wake-strategy.md §3 L2).
		auditH:         newAuditHandlerWithHooks(audit.Handler(auditSink, d.token), reg, combineHooks(wakeHook, stopHook, archMemHook, channelHook, messagingHook, pulseNudgeHook, d.makeUsageHook())),
		plan:           planStore,
		notes:          notesInbox,
		pulse:          pulseInst,
		inFlight:       inFlightLog,
		registry:       reg,
		archMem:        archMemStore,
		archMemCancel:  archMemCancel,
		leaderStatus:   leaderStatusStore,
		leaderURL:      leaderURL,
		registered:     time.Now(),
		transcriptsDir: transcriptsDir,
		repoRoot:       repoRoot,
		channelBus:     chBus,
	}, nil
}

// defaultLeaderStatusPath returns the per-team leader-status board
// file path, alongside plan.jsonl and notes.jsonl.
func defaultLeaderStatusPath(teamID string) string {
	return filepath.Join(defaultStateDir(teamID), "leader_status.json")
}

// defaultMessagingDedupPath returns the daemon-global dedup state file
// (not per-team — messaging is daemon-wide for v1).
func defaultMessagingDedupPath() string {
	return filepath.Join(daemonHomeDir(), "state", "messaging.json")
}

// initMessaging loads ~/.teem/messaging.yaml, resolves credentials from
// the environment, and stashes the resulting Notifier + Dedup on the
// daemon for per-team wiring. Returns an error only when messaging is
// configured-but-broken (enabled with no token / chat_id) — a missing
// config file is the silent off state.
func (d *daemon) initMessaging() error {
	cfg, err := messaging.Load(daemonHomeDir())
	if err != nil {
		return err
	}
	n, err := messaging.Resolve(cfg, os.Getenv)
	if err != nil {
		return err
	}
	if n == nil {
		return nil
	}
	dedup, err := messaging.NewDedup(defaultMessagingDedupPath(), cfg.Telegram.DedupWindow())
	if err != nil {
		// Best-effort: a broken dedup file logs but doesn't block startup.
		// The fresh in-memory map still gates duplicates this run.
		fmt.Fprintf(os.Stderr, "[messaging] dedup state warning: %v\n", err)
	}
	d.messagingNotifier = n
	d.messagingCfg = cfg.Telegram
	d.messagingDedup = dedup
	fmt.Fprintf(os.Stderr, "[teemd] messaging: telegram enabled (chat_id=%d)\n", cfg.Telegram.ChatID)
	return nil
}

// lookupRole returns the role for agentID from the registry, or ""
// when the agent isn't currently tracked. Falls back to parsing the
// instance suffix off the id ("<role>-<N>") because the registry can
// race a worker_stopped reconcile.
func lookupRole(reg *mcpsrv.Registry, agentID string) string {
	if e, ok := reg.Get(agentID); ok && e.Role != "" {
		return e.Role
	}
	if i := strings.LastIndexByte(agentID, '-'); i > 0 {
		return agentID[:i]
	}
	return ""
}

// shortSummary clamps an output string to a single-line preview safe
// for the recent-entries section. Newlines are flattened; the result
// is truncated to 200 bytes.
func shortSummary(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.TrimSpace(s)
	const cap = 200
	if len(s) > cap {
		s = s[:cap] + "…"
	}
	return s
}

// isWakeKind decides whether a worker event should fire a leader.wake
// publish. Different consumers (a future chat banner) may want
// different signals than channels uses.
func isWakeKind(k audit.Kind) bool {
	switch k {
	case audit.KindJobComplete, audit.KindJobError, audit.KindJobTranscriptReady, audit.KindWorkerStopped:
		return true
	}
	return false
}

// channelPushFn is the narrow surface makeChannelHook calls into. The
// production binding is mcpsrv.Server.PushChannel; tests substitute a
// recorder.
type channelPushFn func(body string, meta map[string]string)

// makeChannelHook returns the auditHook that fans selected events out
// to the team MCP server as Claude Code channel notifications. Pulled
// out of buildTeamServices so it can be unit-tested without the rest
// of the per-team plumbing.
func makeChannelHook(push channelPushFn) auditHook {
	return func(events []audit.Event) {
		for _, e := range events {
			if !isChannelKind(e.Kind) {
				continue
			}
			body := formatChannelBody(e)
			meta := map[string]string{
				"agent_id": e.AgentID,
				"kind":     string(e.Kind),
			}
			if e.JobID != "" {
				meta["job_id"] = e.JobID
			}
			if tid, ok := e.Meta["task_id"].(string); ok && tid != "" {
				meta["task_id"] = tid
			}
			push(body, meta)
		}
	}
}

// isChannelKind decides whether an audit event should be pushed into
// the leader's claude channel. The set mirrors the leader-relevant
// signals the dashboard surfaces — terminal job state, blockers,
// recorded decisions, worker shutdown, daemon-killed jobs, and
// pipeline-stage movement — and intentionally excludes high-frequency
// noise (heartbeats, pulse_tick echoes).
func isChannelKind(k audit.Kind) bool {
	switch k {
	case audit.KindJobComplete,
		audit.KindJobError,
		audit.KindJobInterrupted,
		audit.KindBlockerNote,
		audit.KindDecisionNote,
		audit.KindWorkerStopped,
		audit.KindTaskStageChanged:
		return true
	}
	return false
}

// formatChannelBody renders a short, human-readable one-liner for an
// audit event suitable for surfacing inside the leader's claude
// session. Body intentionally stays terse: full detail lives in the
// audit log + query_audit tool, and the channel exists to nudge the
// leader to look.
func formatChannelBody(e audit.Event) string {
	agent := e.AgentID
	if agent == "" {
		agent = "<unknown>"
	}
	taskID := ""
	if tid, ok := e.Meta["task_id"].(string); ok {
		taskID = tid
	}
	switch e.Kind {
	case audit.KindJobComplete:
		return fmt.Sprintf("%s finished job %s", agent, e.JobID)
	case audit.KindJobError:
		msg := strings.TrimSpace(e.Message)
		if msg == "" {
			msg = "(no message)"
		}
		return fmt.Sprintf("%s job %s errored: %s", agent, e.JobID, shortSummary(msg))
	case audit.KindJobInterrupted:
		return fmt.Sprintf("%s's job %s was interrupted", agent, e.JobID)
	case audit.KindWorkerStopped:
		return fmt.Sprintf("%s stopped", agent)
	case audit.KindBlockerNote:
		if taskID != "" {
			return fmt.Sprintf("blocker on task %s: %s", taskID, shortSummary(e.Message))
		}
		return "blocker: " + shortSummary(e.Message)
	case audit.KindDecisionNote:
		if taskID != "" {
			return fmt.Sprintf("decision on task %s: %s", taskID, shortSummary(e.Message))
		}
		return "decision: " + shortSummary(e.Message)
	case audit.KindTaskStageChanged:
		if taskID == "" {
			return shortSummary(e.Message)
		}
		stage, _ := e.Meta["stage"].(string)
		if stage == "" {
			stage, _ = e.Meta["to"].(string)
		}
		from, _ := e.Meta["from"].(string)
		switch {
		case from != "" && stage != "":
			return fmt.Sprintf("task %s: %s → %s", taskID, from, stage)
		case stage != "":
			return fmt.Sprintf("task %s moved to %s", taskID, stage)
		default:
			return "task " + taskID + " stage changed"
		}
	}
	return fmt.Sprintf("%s: %s", e.Kind, shortSummary(e.Message))
}

// usageRecorder returns a pulse.OnUsage callback that forwards every
// pulse-tick usage rollup into the daemon-global Aggregator. Returns
// nil when usage isn't wired so pulse's nil-check disables the path.
func (d *daemon) usageRecorder() func(usage.UsageSummary) {
	if d.usageAgg == nil {
		return nil
	}
	return func(s usage.UsageSummary) {
		if err := d.usageAgg.Record(s); err != nil {
			fmt.Fprintf(os.Stderr, "[teemd] usage: record pulse: %v\n", err)
		}
	}
}

// spawnerQuota returns the daemon's Aggregator as an agent.QuotaChecker
// — or a nil interface when usage is unwired, so the spawner's
// `cfg.UsageQuota != nil` check disables the gate cleanly. Returning
// the typed nil directly would be a non-nil interface value and crash
// the gate check.
func (d *daemon) spawnerQuota() agent.QuotaChecker {
	if d.usageAgg == nil {
		return nil
	}
	return d.usageAgg
}

// makeUsageHook returns the auditHook that feeds the daemon-global
// usage Aggregator. KindUsageEvent events are decoded back into a
// UsageSummary and recorded; everything else passes through.
func (d *daemon) makeUsageHook() auditHook {
	if d.usageAgg == nil {
		return nil
	}
	return func(events []audit.Event) {
		for _, e := range events {
			if e.Kind != audit.KindUsageEvent {
				continue
			}
			d.usageAgg.Record(usageSummaryFromMeta(e))
		}
	}
}

// usageSummaryFromMeta reconstructs a usage.UsageSummary from a
// KindUsageEvent's Meta bag. The Meta wire shape comes from
// usage.AuditMeta; missing/typed-incorrectly fields fall back to
// zero so a malformed event is just under-counted, not fatal.
func usageSummaryFromMeta(e audit.Event) usage.UsageSummary {
	m := e.Meta
	s := usage.UsageSummary{
		Model:             metaString(m, "model"),
		InputTokens:       metaInt64(m, "input_tokens"),
		OutputTokens:      metaInt64(m, "output_tokens"),
		CacheCreateTokens: metaInt64(m, "cache_create_tokens"),
		CacheReadTokens:   metaInt64(m, "cache_read_tokens"),
	}
	if t, err := time.Parse(time.RFC3339, metaString(m, "started_at")); err == nil {
		s.StartedAt = t
	}
	if t, err := time.Parse(time.RFC3339, metaString(m, "ended_at")); err == nil {
		s.EndedAt = t
	}
	if s.EndedAt.IsZero() {
		s.EndedAt = e.Timestamp
	}
	return s
}

func metaString(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

// metaInt64 tolerates JSON's float64 round-trip plus the int64 type the
// in-process emitter uses directly.
func metaInt64(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

// onUsageThrottle is the Aggregator's transition callback. Each
// active↔throttled flip is fanned out to every team's audit sink so
// each leader can react locally. Best-effort: failures are logged but
// don't propagate.
func (d *daemon) onUsageThrottle(ev usage.ThrottleEvent) {
	meta := map[string]any{
		"state":  ev.State,
		"used":   ev.Used,
		"cap":    ev.Cap,
		"reason": ev.Reason,
	}
	d.mu.Lock()
	teams := make([]*registeredTeam, 0, len(d.teams))
	for _, rt := range d.teams {
		teams = append(teams, rt)
	}
	d.mu.Unlock()
	for _, rt := range teams {
		if rt.auditSink == nil {
			continue
		}
		if err := rt.auditSink.Write(audit.Event{
			Timestamp: time.Now().UTC(),
			AgentID:   "leader",
			Kind:      audit.KindUsageThrottle,
			Meta:      meta,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "[teemd] usage: throttle audit write %s: %v\n", rt.team.Name, err)
		}
	}
}

// combineHooks chains any number of audit hooks left-to-right. nil
// entries are skipped; returns nil only if every input is nil.
func combineHooks(hooks ...auditHook) auditHook {
	// hooks[:0] reuses the variadic backing array; safe today because no
	// caller passes a slice with `...`. If that changes, copy into a fresh
	// slice instead — silent mutation of a caller's slice is a footgun.
	live := hooks[:0]
	for _, h := range hooks {
		if h != nil {
			live = append(live, h)
		}
	}
	if len(live) == 0 {
		return nil
	}
	if len(live) == 1 {
		return live[0]
	}
	chained := make([]auditHook, len(live))
	copy(chained, live)
	return func(events []audit.Event) {
		for _, h := range chained {
			h(events)
		}
	}
}

// auditHook is a side-channel callback invoked on every accepted
// audit POST. Used by Pulse to schedule debounced event-triggered
// ticks. nil is fine — the handler just skips the call.
type auditHook func(events []audit.Event)

// newAuditHandlerWithHooks wraps an audit handler so each accepted
// POST body is parsed once and its events get fanned out to the
// registry (LastSeen update) and any extra hook. The inner audit
// Handler is the actual responder.
func newAuditHandlerWithHooks(inner http.Handler, reg *mcpsrv.Registry, hook auditHook) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err == nil {
				var events []audit.Event
				if json.Unmarshal(body, &events) == nil {
					now := time.Now().UTC()
					for _, e := range events {
						ts := e.Timestamp
						if ts.IsZero() {
							ts = now
						}
						reg.SetLastSeen(e.AgentID, ts)
					}
					if hook != nil {
						hook(events)
					}
				}
				r.Body = newBytesReadCloser(body)
			}
		}
		inner.ServeHTTP(w, r)
	})
}

// newBytesReadCloser returns an io.ReadCloser that hands out the
// supplied bytes. Used to restore r.Body after we read it for the
// audit tee.
func newBytesReadCloser(body []byte) io.ReadCloser {
	return &bytesReadCloser{r: strings.NewReader(string(body))}
}

type bytesReadCloser struct{ r *strings.Reader }

func (b *bytesReadCloser) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *bytesReadCloser) Close() error               { return nil }

func writeTempYAML(body string) (string, error) {
	f, err := os.CreateTemp("", "teem-register-*.yaml")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(body); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// --- /teams/<name>/* handlers ---------------------------------------------

// resolveTeam looks a team up by its canonical id (the `t-<hex>`
// routing key) or, as a fallback, by its display name. The id match
// always wins, so when two teams happen to share a Name (rare, since
// init/register checks for it) the alias is best-effort and won't
// shadow an id lookup. Returns nil if no team matches.
//
// The name alias exists so URLs that long-lived clients captured
// before the T33 / TI1 migration — when the daemon keyed `d.teams`
// by t.Name — still resolve after a restart. Concretely: a `teem
// chat` Claude Code subprocess holds a stale `/teams/<old-name>/mcp`
// URL; the alias keeps that handshake alive instead of forcing a
// reconnect.
func (d *daemon) resolveTeam(key string) *registeredTeam {
	d.mu.Lock()
	defer d.mu.Unlock()
	if rt, ok := d.teams[key]; ok {
		return rt
	}
	for _, rt := range d.teams {
		if rt.team != nil && rt.team.Name == key {
			return rt
		}
	}
	return nil
}

// handleTeamRoute dispatches /teams/<id>/(mcp|audit) to the matching
// per-team handler after stripping the team prefix from the request
// path. The MCP handler is path-agnostic; the audit handler reads
// "/audit" so we rewrite the URL.Path accordingly.
func (d *daemon) handleTeamRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/teams/")
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		// Bare /teams/<id> — render the per-team detail SSR page.
		if rest == "" {
			http.NotFound(w, r)
			return
		}
		d.renderTeamPage(w, r, rest)
		return
	}
	id, suffix := rest[:slash], rest[slash:]
	rt := d.resolveTeam(id)
	if rt == nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasPrefix(suffix, "/mcp"):
		// Forward to the team's MCP handler. It expects to see /mcp at
		// the root.
		r2 := r.Clone(r.Context())
		r2.URL.Path = suffix
		rt.mcp.Handler().ServeHTTP(w, r2)
	case suffix == "/audit" || strings.HasPrefix(suffix, "/audit?") || suffix == "/audit/":
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/audit"
		rt.auditH.ServeHTTP(w, r2)
	case strings.HasPrefix(suffix, "/transcripts/"):
		d.handleTranscripts(w, r, rt, strings.TrimPrefix(suffix, "/transcripts/"))
	case suffix == "/channel-events" || strings.HasPrefix(suffix, "/channel-events?"):
		d.handleChannelEvents(w, r, rt)
	default:
		// SSR jobs pages — unauth like the dashboard (tailnet boundary).
		if agentID, ok := resolveAgentJobsRoute(suffix); ok {
			d.renderAgentJobs(w, r, rt, agentID)
			return
		}
		if taskID, action, ok := resolveTaskActionRoute(suffix); ok {
			d.handleTaskActionForm(w, r, rt, taskID, action)
			return
		}
		if taskID, action, ok := resolveDecisionActionRoute(suffix); ok {
			d.handleDecisionActionForm(w, r, rt, taskID, action)
			return
		}
		if taskID, ok := resolveTaskFlowRoute(suffix); ok {
			d.renderTaskFlow(w, r, rt, taskID)
			return
		}
		if jobID, ok := resolveJobDetailRoute(suffix); ok {
			d.renderJobDetail(w, r, rt, jobID)
			return
		}
		http.NotFound(w, r)
	}
}

// validIDRegexp matches the agent_id / job_id forms accepted on the
// transcripts route. Restricted to letters, digits, and `.`/`_`/`-`
// so URL handlers can't be tricked into writing outside the team's
// transcripts directory. isSafeID layers on top of the regex to
// reject the dot-only literals `.` and `..` which the character class
// would otherwise accept and which filepath.Join resolves to a path
// escape.
var validIDRegexp = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func isSafeID(s string) bool {
	return validIDRegexp.MatchString(s) && s != "." && s != ".."
}

// handleTranscripts implements GET/POST /teams/<name>/transcripts/<agent>/<job>.
// Bearer-auth gated (same shared token as /audit). POST writes the body
// to the team's transcripts mirror; GET serves it back. ?head=N on GET
// returns the first N NDJSON events (lines) rather than the whole body.
func (d *daemon) handleTranscripts(w http.ResponseWriter, r *http.Request, rt *registeredTeam, rest string) {
	if r.Header.Get("Authorization") != "Bearer "+d.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		http.Error(w, "want /transcripts/<agent_id>/<job_id>", http.StatusBadRequest)
		return
	}
	agentID, jobID := rest[:slash], rest[slash+1:]
	if !isSafeID(agentID) || !isSafeID(jobID) {
		http.Error(w, "bad agent_id or job_id (must match [A-Za-z0-9._-]+ and not be . or ..)", http.StatusBadRequest)
		return
	}
	if rt.transcriptsDir == "" {
		http.Error(w, "transcripts not configured", http.StatusInternalServerError)
		return
	}
	path := filepath.Join(rt.transcriptsDir, agentID, jobID+".jsonl")
	switch r.Method {
	case http.MethodPost:
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			http.Error(w, "mkdir: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// MaxBytesReader (not LimitReader) — surfaces the cap as a
		// returned error so we can 413 instead of silently truncating
		// a too-large upload.
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024*1024)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				http.Error(w, "transcript too large (>64 MiB)", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := atomicWrite(path, body); err != nil {
			http.Error(w, "write: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		body, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "read: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		if h := r.URL.Query().Get("head"); h != "" {
			n, err := strconv.Atoi(h)
			if err != nil || n < 0 {
				http.Error(w, "bad head", http.StatusBadRequest)
				return
			}
			body = headLines(body, n)
		}
		_, _ = w.Write(body)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// observeChannelSubscribe subscribes a new SSE handler to the team's
// channelbus and drives the channels-live ↔ fallback state machine.
// It is the single place that mutates rt.channelsLive: the flag, the
// audit transition event, and the pulse gate are all flipped together
// under detectionMu, so concurrent connect/disconnect races converge
// on a single transition emission. The returned cancel unsubscribes
// and runs the symmetric fallback transition when the last subscriber
// leaves.
//
// Why the mutex wraps cancel() (not just the bool flip): without it,
// a concurrent subscribe between cancel()'s "I am the last" snapshot
// and the flag mutation would see channelsLive=true and skip its own
// 0→1 transition, then this cancel would clear the flag and emit
// "fallback" while the new subscriber is in fact live. Holding
// detectionMu across both the channelbus mutation AND the flag
// mutation linearizes the decisions.
func (d *daemon) observeChannelSubscribe(rt *registeredTeam) (<-chan channelbus.Event, func()) {
	if rt.channelBus == nil {
		closed := make(chan channelbus.Event)
		close(closed)
		return closed, func() {}
	}
	_, ch, count, cancelSub := rt.channelBus.SubscribeAndCount()
	rt.detectionMu.Lock()
	if count == 1 && !rt.channelsLive {
		rt.channelsLive = true
		if rt.pulse != nil {
			rt.pulse.SetChannelsLive(true)
		}
		if rt.auditSink != nil {
			_ = rt.auditSink.Write(audit.Event{
				Timestamp: time.Now().UTC(),
				AgentID:   "leader",
				Kind:      audit.KindChannelsState,
				Meta:      map[string]any{"state": "live", "team": rt.team.ID},
			})
		}
	}
	rt.detectionMu.Unlock()
	cancel := func() {
		rt.detectionMu.Lock()
		defer rt.detectionMu.Unlock()
		post := cancelSub()
		if post == 0 && rt.channelsLive {
			rt.channelsLive = false
			if rt.pulse != nil {
				rt.pulse.SetChannelsLive(false)
			}
			if rt.auditSink != nil {
				_ = rt.auditSink.Write(audit.Event{
					Timestamp: time.Now().UTC(),
					AgentID:   "leader",
					Kind:      audit.KindChannelsState,
					Meta:      map[string]any{"state": "fallback", "team": rt.team.ID},
				})
			}
		}
	}
	return ch, cancel
}

// handleChannelEvents serves the team's channel-event SSE stream to a
// teem-channel stdio shim. The shim runs as a subprocess of Claude
// Code, holds open one GET against this endpoint, and re-emits every
// received Event as a notifications/claude/channel message on its
// stdio MCP transport — which is what claude actually listens on for
// channels. Bearer-auth (same worker_token as /audit).
//
// Event wire format: SSE frames with event name "channel" and a JSON
// data line { "content": "...", "meta": { "agent_id": "...", ... } }.
// A periodic ":keepalive\n\n" comment keeps the connection through
// upstream idle timeouts.
func (d *daemon) handleChannelEvents(w http.ResponseWriter, r *http.Request, rt *registeredTeam) {
	if r.Header.Get("Authorization") != "Bearer "+d.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if rt.channelBus == nil {
		http.Error(w, "channel bus not configured", http.StatusInternalServerError)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := d.observeChannelSubscribe(rt)
	defer cancel()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if _, err := io.WriteString(w, ":keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: channel\ndata: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// headLines returns the first n NDJSON lines from body (preserving
// trailing newlines). n <= 0 returns body unchanged.
func headLines(body []byte, n int) []byte {
	if n <= 0 {
		return body
	}
	count := 0
	for i, c := range body {
		if c == '\n' {
			count++
			if count == n {
				return body[:i+1]
			}
		}
	}
	return body
}

// --- state snapshot --------------------------------------------------------

// persistStateSnapshot refreshes daemon.json with the current set of
// runRetentionGC ticks on cfg.SweepInterval and, for each registered
// team, removes stopped registry entries older than cfg.StoppedAgentTTL
// and transcript files older than cfg.TranscriptTTL. Default
// configuration ("never delete") prevents this goroutine from being
// started in the first place — see serveDaemon's retCfg.Enabled() guard.
//
// The first sweep runs 30s after startup so a developer can observe
// whether the configured TTL is sane without waiting an hour. Subsequent
// sweeps fire on the configured interval (default 1h).
func (d *daemon) runRetentionGC(cfg retention.Config) {
	interval := retentionSweepInterval(cfg)
	select {
	case <-d.baseCtx.Done():
		return
	case <-time.After(30 * time.Second):
	}
	d.retentionSweep(cfg)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-d.baseCtx.Done():
			return
		case <-t.C:
			d.retentionSweep(cfg)
		}
	}
}

// retentionSweep runs one pass across every registered team, logging
// counts only when something was actually removed so the log stays
// quiet on idle systems.
func (d *daemon) retentionSweep(cfg retention.Config) {
	d.mu.Lock()
	teams := make([]*registeredTeam, 0, len(d.teams))
	for _, rt := range d.teams {
		teams = append(teams, rt)
	}
	d.mu.Unlock()
	now := time.Now()
	for _, rt := range teams {
		if cfg.StoppedAgentTTL > 0 && rt.registry != nil {
			if removed := rt.registry.GCStopped(now, cfg.StoppedAgentTTL); removed > 0 {
				fmt.Fprintf(os.Stderr, "[retention] %s: pruned %d stopped agent(s) older than %s\n",
					rt.team.Name, removed, cfg.StoppedAgentTTL)
			}
		}
		if cfg.TranscriptTTL > 0 && rt.transcriptsDir != "" {
			removed, err := retention.SweepTranscripts(rt.transcriptsDir, now, cfg.TranscriptTTL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[retention] %s: transcript sweep error: %v\n", rt.team.Name, err)
			}
			if removed > 0 {
				fmt.Fprintf(os.Stderr, "[retention] %s: removed %d transcript file(s) older than %s\n",
					rt.team.Name, removed, cfg.TranscriptTTL)
			}
		}
	}
}

// retentionSweepInterval returns the effective sweep cadence, falling
// back to retention.DefaultSweepInterval when unset.
func retentionSweepInterval(cfg retention.Config) time.Duration {
	if cfg.SweepInterval > 0 {
		return cfg.SweepInterval
	}
	return retention.DefaultSweepInterval
}

// registered teams. Called after every registration/unregistration so
// `teem status` sees up-to-date info.
func (d *daemon) persistStateSnapshot() {
	d.mu.Lock()
	names := make([]string, 0, len(d.teams))
	for _, rt := range d.teams {
		names = append(names, rt.team.Name)
	}
	d.mu.Unlock()
	_ = writeDaemonStateFile(daemonStateFile{
		PID:       os.Getpid(),
		Endpoint:  d.endpoint,
		Token:     d.token,
		Teams:     names,
		StartedAt: time.Now().UTC(),
	})
}

// --- detached fork ---------------------------------------------------------

// forkDetached re-execs the current binary with `start --detached`,
// redirecting stdio to ~/.teem/daemon.log and starting a new session
// so the child outlives the parent.
func forkDetached(df *daemonFlags) error {
	logPath := daemonLogPath()
	if err := os.MkdirAll(daemonHomeDir(), 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer logFile.Close()

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}
	childArgs := []string{"start", "--detached", "--listen", df.listenAddr}
	if !df.useTailnet {
		childArgs = append(childArgs, "--tailnet=false")
	}
	cmd := exec.Command(self, childArgs...)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn detached daemon: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	fmt.Fprintf(os.Stderr, "[teem] daemon spawned (pid %d, log: %s)\n", pid, logPath)
	return nil
}

// --- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
