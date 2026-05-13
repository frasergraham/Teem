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
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
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
	endpoint   string // public URL: http://host:port
	token      string // shared bearer for /audit and /control

	mu    sync.Mutex
	teams map[string]*registeredTeam
}

type registeredTeam struct {
	team       *team.Team
	mcp        *mcpsrv.Server
	spawner    *agent.Spawner
	auditSink  *audit.FileSink
	auditH     http.Handler
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

	d := &daemon{teams: map[string]*registeredTeam{}}

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
		ctx2, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx2)
		// Tear down each team's spawner so ephemeral workers exit.
		d.mu.Lock()
		for _, rt := range d.teams {
			rt.spawner.Stop()
			_ = rt.auditSink.Close()
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
	name := strings.TrimPrefix(r.URL.Path, "/control/teams/")
	if name == "" || strings.Contains(name, "/") {
		http.Error(w, "bad team name", http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.mu.Lock()
	rt, ok := d.teams[name]
	if ok {
		delete(d.teams, name)
	}
	d.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	rt.spawner.Stop()
	_ = rt.auditSink.Close()
	d.persistStateSnapshot()
	w.WriteHeader(http.StatusNoContent)
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

	bs := bus.NewMemBus()
	reg := mcpsrv.NewRegistry()
	spawner := agent.NewSpawner(t, bs, reg, agent.Config{
		HTTPClient:       d.httpClient,
		WorkerToken:      d.token,
		CloudProvisioner: cloudProvisionerFactory(d.token, leaderURL, gitCfg, stateStore),
		RepoRoot:         repoRoot,
		WorktreeBase:     worktreeBase,
		LeaderURL:        leaderURL,
		StateStore:       stateStore,
	})

	auditPath := defaultAuditPath(t.Name)
	auditSink, err := audit.OpenFile(auditPath)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}

	srv, err := mcpsrv.New(mcpsrv.Config{
		Bus:      bs,
		Team:     t,
		Registry: reg,
		Spawner:  spawner,
		Audit:    auditSink,
	})
	if err != nil {
		_ = auditSink.Close()
		return nil, err
	}

	return &registeredTeam{
		team:       t,
		mcp:        srv,
		spawner:    spawner,
		auditSink:  auditSink,
		auditH:     audit.Handler(auditSink, d.token),
		leaderURL:  leaderURL,
		registered: time.Now(),
	}, nil
}

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
