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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/frasergraham/teem/internal/agent"
	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/inflight"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/notes"
	"github.com/frasergraham/teem/internal/plan"
	"github.com/frasergraham/teem/internal/pulse"
	"github.com/frasergraham/teem/internal/state"
	"github.com/frasergraham/teem/internal/tailnet"
	"github.com/frasergraham/teem/internal/team"
)

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

// runTeams prints the registered teams by querying the daemon's
// /control/teams endpoint. Useful from any cwd.
func runTeams(args []string) error {
	fs := flag.NewFlagSet("teams", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, alive := readDaemonPID()
	if !alive {
		fmt.Println("daemon not running")
		return nil
	}
	s, ok, err := readDaemonStateFile()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("daemon state file missing")
	}
	// Read live state from the daemon's control endpoint — survives
	// staleness in the state file.
	resp, err := http.Get(s.Endpoint + "/control/teams")
	if err != nil {
		return fmt.Errorf("contact daemon: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("daemon returned %s", resp.Status)
	}
	body, _ := io.ReadAll(resp.Body)
	fmt.Println(string(body))
	return nil
}

// --- daemon process: top-level state file ---------------------------------

// daemonStateFile is the on-disk endpoint discovery file at
// ~/.teem/daemon.json. teem chat / teem status / teem teams read it.
type daemonStateFile struct {
	PID       int       `json:"pid"`
	Endpoint  string    `json:"endpoint"`            // http://<host>:<port>
	Token     string    `json:"worker_token"`        // shared bearer for /audit and /control
	Teams     []string  `json:"teams,omitempty"`     // registered team names
	StartedAt time.Time `json:"started_at"`
}

func daemonHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".teem"
	}
	return filepath.Join(home, ".teem")
}

func daemonPIDPath() string     { return filepath.Join(daemonHomeDir(), "daemon.pid") }
func daemonJSONPath() string    { return filepath.Join(daemonHomeDir(), "daemon.json") }
func daemonLogPath() string     { return filepath.Join(daemonHomeDir(), "daemon.log") }

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

	mu    sync.Mutex
	teams map[string]*registeredTeam
}

type registeredTeam struct {
	team       *team.Team
	mcp        *mcpsrv.Server
	spawner    *agent.Spawner
	auditSink  *audit.FileSink
	auditH     http.Handler
	plan       *plan.Plan
	notes      *notes.Inbox
	pulse      *pulse.Pulse
	inFlight   *inflight.Log
	registry   *mcpsrv.Registry
	leaderURL  string
	registered time.Time
}

// serveDaemon runs the multi-tenant orchestrator until ctx is cancelled.
// Teams are registered lazily via POST /control/teams.
func serveDaemon(ctx context.Context, df *daemonFlags) error {
	hostname := os.Getenv("TEEM_DAEMON_HOSTNAME")
	if hostname == "" {
		hostname = "teem-daemon"
	}

	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: ctx}

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
		d.token = randomToken()
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
	fmt.Fprintf(os.Stderr, "[teemd] ready. Stop with `teem stop` or kill %d\n", os.Getpid())

	httpSrv := &http.Server{
		Handler:           d.handler(),
		ReadHeaderTimeout: 10 * time.Second,
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
			rt.pulse.Stop()
			rt.spawner.Stop()
			_ = rt.auditSink.Close()
			_ = rt.plan.Close()
			_ = rt.notes.Close()
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
		case strings.HasPrefix(path, "/control/teams/"):
			d.requireAuth(w, r, d.handleControlTeamsItem)
		case strings.HasPrefix(path, "/teams/"):
			d.handleTeamRoute(w, r)
		default:
			http.NotFound(w, r)
		}
	})
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
		http.Error(w, "bad team name", http.StatusBadRequest)
		return
	}
	// Split into <name> and optional <subresource>.
	name := rest
	sub := ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		name, sub = rest[:i], rest[i+1:]
	}
	d.mu.Lock()
	rt, ok := d.teams[name]
	d.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch sub {
	case "":
		// Whole-team operations.
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		d.mu.Lock()
		delete(d.teams, name)
		d.mu.Unlock()
		rt.pulse.Stop()
		rt.spawner.Stop()
		_ = rt.auditSink.Close()
		_ = rt.plan.Close()
		_ = rt.notes.Close()
		if rt.inFlight != nil {
			_ = rt.inFlight.Close()
		}
		d.persistStateSnapshot()
		w.WriteHeader(http.StatusNoContent)
	case "pulse":
		d.handlePulseControl(w, r, rt)
	default:
		http.NotFound(w, r)
	}
}

// pulseStatus is the GET response shape and the start-result shape.
type pulseStatus struct {
	Running   bool      `json:"running"`
	Paused    bool      `json:"paused"`
	Interval  string    `json:"interval"`
	LastTick  time.Time `json:"last_tick,omitempty"`
	TickCount int64     `json:"tick_count"`
}

// pulseCommand is the POST body for action-style requests.
type pulseCommand struct {
	Action   string `json:"action"`   // start|stop|pause|resume|tick
	Interval string `json:"interval"` // for start; Go duration string
	Reason   string `json:"reason"`   // for pause
}

// handlePulseControl handles GET/POST under /control/teams/<name>/pulse.
// The control plane is intentionally small: the daemon does the work,
// the CLI is a thin formatter.
func (d *daemon) handlePulseControl(w http.ResponseWriter, r *http.Request, rt *registeredTeam) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, currentPulseStatus(rt))
	case http.MethodPost:
		var cmd pulseCommand
		if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil && err != io.EOF {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		switch cmd.Action {
		case "start":
			if cmd.Interval != "" {
				dur, err := time.ParseDuration(cmd.Interval)
				if err != nil {
					http.Error(w, "bad interval: "+err.Error(), http.StatusBadRequest)
					return
				}
				rt.pulse.SetInterval(dur)
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
			go func() { _ = rt.pulse.Tick(d.baseCtx, "manual") }()
		default:
			http.Error(w, "unknown action: "+cmd.Action, http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, currentPulseStatus(rt))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func currentPulseStatus(rt *registeredTeam) pulseStatus {
	return pulseStatus{
		Running:   rt.pulse.Running(),
		Paused:    rt.pulse.Paused(),
		Interval:  rt.pulse.Interval().String(),
		LastTick:  rt.pulse.LastTick(),
		TickCount: rt.pulse.TickCount(),
	}
}

func (d *daemon) handleListTeams(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]map[string]any, 0, len(d.teams))
	for name, rt := range d.teams {
		out = append(out, map[string]any{
			"name":       name,
			"mcp_url":    d.endpoint + "/teams/" + name + "/mcp",
			"audit_url":  d.endpoint + "/teams/" + name + "/audit",
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
	// Parse the YAML by writing to a temp file and using team.Load — it
	// already validates everything we need.
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

	// Idempotent: re-registering an existing team is a no-op that
	// returns the same URLs. (Trade-off: we don't pick up YAML edits
	// without an explicit DELETE first. Document.)
	d.mu.Lock()
	existing, ok := d.teams[t.Name]
	d.mu.Unlock()
	if ok {
		writeJSON(w, http.StatusOK, registerResponse{
			Team:     existing.team.Name,
			MCPURL:   d.endpoint + "/teams/" + t.Name + "/mcp",
			AuditURL: d.endpoint + "/teams/" + t.Name + "/audit",
		})
		return
	}

	rt, err := d.buildTeamServices(t, req.RepoRoot, req.WorktreeBase)
	if err != nil {
		http.Error(w, fmt.Sprintf("build team services: %v", err), http.StatusInternalServerError)
		return
	}
	d.mu.Lock()
	d.teams[t.Name] = rt
	d.mu.Unlock()
	d.persistStateSnapshot()

	// Best-effort reconcile of persistent agents — done after
	// registration so list_agents reflects them straight away.
	go func() {
		if n := rt.spawner.Reconcile(context.Background()); n > 0 {
			fmt.Fprintf(os.Stderr, "[teemd] reconciled %d persistent agent(s) for %s\n", n, t.Name)
		}
	}()

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
	if worktreeBase == "" {
		worktreeBase = defaultWorktreeBase(t.Name)
	}
	leaderURL := d.endpoint + "/teams/" + t.Name
	stateStore := state.NewStore(defaultStateDir(t.Name))
	gitCfg := readGitConfig()

	auditPath := defaultAuditPath(t.Name)
	auditSink, err := audit.OpenFile(auditPath)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}

	planPath := defaultPlanPath(t.Name)
	planStore, err := plan.Open(planPath)
	if err != nil {
		_ = auditSink.Close()
		return nil, fmt.Errorf("plan: %w", err)
	}

	notesInbox, err := notes.Open(defaultNotesPath(t.Name))
	if err != nil {
		_ = auditSink.Close()
		_ = planStore.Close()
		return nil, fmt.Errorf("notes: %w", err)
	}

	// In-flight log for durability. Opened before reconcile so the
	// next steps can both (a) emit job_interrupted for orphans and
	// (b) hand it to the spawner for future jobs.
	inFlightLog, err := inflight.Open(defaultInFlightPath(t.Name))
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
	spawner := agent.NewSpawner(d.baseCtx, t, bs, reg, agent.Config{
		HTTPClient:       d.httpClient,
		WorkerToken:      d.token,
		CloudProvisioner: cloudProvisionerFactory(d.token, leaderURL, gitCfg, stateStore),
		RepoRoot:         repoRoot,
		WorktreeBase:     worktreeBase,
		LeaderURL:        leaderURL,
		StateStore:       stateStore,
		AuditSink:        auditSink,
		ArchetypeSeqPath: defaultArchetypeSeqPath(t.Name),
		InFlight:         inFlightLog,
	})

	srv, err := mcpsrv.New(mcpsrv.Config{
		Bus:      bs,
		Team:     t,
		Registry: reg,
		Spawner:  spawner,
		Audit:    auditSink,
		Plan:     planStore,
		Notes:    notesInbox,
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
	pulseMCPPath := filepath.Join(defaultStateDir(t.Name), "pulse-mcp.json")
	if err := pulse.WriteMCPConfig(pulseMCPPath, leaderURL+"/mcp"); err != nil {
		_ = auditSink.Close()
		_ = planStore.Close()
		_ = notesInbox.Close()
		return nil, fmt.Errorf("pulse mcp config: %w", err)
	}
	pulseInst := pulse.New(pulse.Config{
		TeamName: t.Name,
		LoadSession: func() (string, bool, error) {
			s, ok, err := loadLeaderSession(t.Name)
			if err != nil || !ok {
				return "", false, err
			}
			return s.SessionID, true, nil
		},
		PauseFile:   filepath.Join(defaultStateDir(t.Name), "pulse.paused"),
		RunningFile: defaultPulseRunningFlag(t.Name),
		MCPConfig:   pulseMCPPath,
		RepoRoot:    repoRoot,
		Plan:        planStore,
		Audit:       auditSink,
		Registry:    reg,
	})
	// Auto-resume Pulse if it was running before the daemon
	// restarted. Operator opt-out is `teem pulse stop` (which clears
	// the flag) or `teem pulse pause` (which leaves it alone but
	// skips ticks).
	if pulseInst.WasRunning() {
		pulseInst.Start(d.baseCtx)
		fmt.Fprintf(os.Stderr, "[teemd] auto-resumed Pulse for %q\n", t.Name)
	}

	return &registeredTeam{
		team:      t,
		mcp:       srv,
		spawner:   spawner,
		auditSink: auditSink,
		// Audit handler fans every POST out to: write to disk, bump
		// the agent's LastSeen, AND nudge Pulse so an event-triggered
		// tick can fire after the debounce window.
		auditH:     newAuditHandlerWithHooks(audit.Handler(auditSink, d.token), reg, pulseInst.NudgeFromAudit),
		plan:       planStore,
		notes:      notesInbox,
		pulse:      pulseInst,
		inFlight:   inFlightLog,
		registry:   reg,
		leaderURL:  leaderURL,
		registered: time.Now(),
	}, nil
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

// handleTeamRoute dispatches /teams/<name>/(mcp|audit) to the matching
// per-team handler after stripping the team prefix from the request
// path. The MCP handler is path-agnostic; the audit handler reads
// "/audit" so we rewrite the URL.Path accordingly.
func (d *daemon) handleTeamRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/teams/")
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		http.NotFound(w, r)
		return
	}
	name, suffix := rest[:slash], rest[slash:]
	d.mu.Lock()
	rt, ok := d.teams[name]
	d.mu.Unlock()
	if !ok {
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
	default:
		http.NotFound(w, r)
	}
}

// --- state snapshot --------------------------------------------------------

// persistStateSnapshot refreshes daemon.json with the current set of
// registered teams. Called after every registration/unregistration so
// `teem status` sees up-to-date info.
func (d *daemon) persistStateSnapshot() {
	d.mu.Lock()
	names := make([]string, 0, len(d.teams))
	for name := range d.teams {
		names = append(names, name)
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
