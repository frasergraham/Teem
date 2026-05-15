// Command teem-worker is the daemon that runs inside a remote worker
// container (today: ECS Fargate). It joins the same tailnet as the leader,
// exposes a small HTTP API gated by a shared bearer token, and runs
// `claude -p ...` for each accepted job.
//
// All routes require Authorization: Bearer $TEEM_WORKER_TOKEN.
//
//	POST /jobs              {job_id, prompt, context, mcps}  → 202
//	GET  /jobs/{id}?wait=Ns  → {status, output, error}        (long-poll)
//	GET  /healthz            → {ok, hostname, agent_id, jobs_in_flight}
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/executor"
	"github.com/frasergraham/teem/internal/gitsetup"
	"github.com/frasergraham/teem/internal/tailnet"
	"github.com/frasergraham/teem/internal/team"
	"github.com/frasergraham/teem/internal/transport"
)

type jobStatus string

const (
	statusPending jobStatus = "pending"
	statusRunning jobStatus = "running"
	statusDone    jobStatus = "done"
	statusError   jobStatus = "error"
)

// jobRecord is the in-memory state for one job. waitCh is closed when the
// status reaches a terminal value, used to implement long-poll.
type jobRecord struct {
	mu       sync.Mutex
	status   jobStatus
	output   string
	errMsg   string
	finished time.Time
	doneCh   chan struct{}
}

func newJobRecord() *jobRecord {
	return &jobRecord{status: statusPending, doneCh: make(chan struct{})}
}

func (r *jobRecord) finish(output, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if errMsg != "" {
		r.status = statusError
	} else {
		r.status = statusDone
	}
	r.output = output
	r.errMsg = errMsg
	r.finished = time.Now()
	select {
	case <-r.doneCh:
	default:
		close(r.doneCh)
	}
}

func (r *jobRecord) snapshot() (jobStatus, string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status, r.output, r.errMsg
}

// workerState tracks the lifecycle of a self-exiting worker. The
// transitions are one-way: serving → draining → shutdown.
//
//   - serving: accepts /jobs POSTs normally.
//   - draining: rejects new jobs with 409; armed exit timer is pending.
//   - shutdown: pre-exit hooks and worker_stopped emit are running or
//     done; the server is about to close.
type workerState int

const (
	stateServing workerState = iota
	stateDraining
	stateShutdown
)

func (s workerState) String() string {
	switch s {
	case stateServing:
		return "serving"
	case stateDraining:
		return "draining"
	case stateShutdown:
		return "shutdown"
	}
	return "unknown"
}

type worker struct {
	agentID       string
	role          string
	hostname      string
	token         string
	workingDir    string
	transcriptDir string
	exec          executor.Executor
	outbox        *outbox
	// leaderURL is the (trailing-slash-stripped) leader endpoint used
	// for one-shot uploads like transcript bodies. The outbox uses its
	// own client/url internally.
	leaderURL string

	// Git auto-push wiring; populated when TEEM_GIT_REPO_URL was set at
	// startup. gitAutoPush is the resolved boolean (true only when both
	// the repo is configured and auto-push isn't disabled by the
	// operator).
	gitOpts     gitsetup.Options
	gitAutoPush bool

	// exitAfterIdle, when > 0, arms a self-exit timer after every job
	// completion. The worker quits if inFlight stays at 0 for this
	// long. 0 disables — worker stays up until /shutdown or signal.
	exitAfterIdle time.Duration
	// startedAt is the moment Start() finished setting up; emitted in
	// worker_stopped meta as uptime_s.
	startedAt time.Time

	// shutdownCh is closed to ask the server goroutine to gracefully
	// stop. Closed by either the /shutdown handler or the self-exit
	// path. Replaces the old local var in run().
	shutdownCh chan struct{}

	// preExitHooks run inside fireDrain before worker_stopped is
	// emitted. Each gets a 10s ctx; errors are logged but don't block
	// exit. Order is registration order. Hooks run sequentially under
	// no lock so they may use the worker's outbox.
	preExitHooks []func(context.Context) error

	mu         sync.Mutex
	jobs       map[string]*jobRecord
	inFlight   atomic.Int64
	state      workerState
	drainTimer *time.Timer
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "teem-worker:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("teem-worker", flag.ExitOnError)
	noTailnet := fs.Bool("no-tailnet", false, "skip tsnet; bind on 127.0.0.1 (smoke testing)")
	port := fs.String("port", "", "listen port (overrides TEEM_LISTEN_PORT; default :7780)")
	unixSocket := fs.String("unix-socket", "", "listen on a unix socket at this path instead of tcp; overrides --no-tailnet and tailnet")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *unixSocket == "" {
		*unixSocket = os.Getenv("TEEM_UNIX_SOCKET")
	}

	cfg, err := loadConfig(*port)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// If the operator wired a git repo, clone it into <workdir>/repo and
	// place the worker on its dedicated branch. The executor's working
	// directory becomes the clone path so claude sees the checkout.
	jobCwd := cfg.workingDir
	var gitOpts gitsetup.Options
	if cfg.gitRepoURL != "" {
		gitOpts = gitsetup.Options{
			AgentID:      cfg.agentID,
			WorkDir:      cfg.workingDir,
			RepoURL:      cfg.gitRepoURL,
			Token:        cfg.gitToken,
			Username:     cfg.gitUsername,
			AuthorName:   cfg.gitAuthorName,
			AuthorEmail:  cfg.gitAuthorEmail,
			BranchPrefix: cfg.gitBranchPrefix,
		}
		clone, err := gitsetup.Configure(ctx, gitOpts)
		if err != nil {
			return fmt.Errorf("git setup: %w", err)
		}
		jobCwd = clone
		fmt.Fprintf(os.Stderr, "[teem-worker] git: cloned %s into %s on branch %s\n", cfg.gitRepoURL, clone, gitsetup.Branch(gitOpts))
	}

	// Outbox + transcript paths share a common .teem-events directory
	// rooted at the worker's workdir (so they get cleaned up when the
	// workdir does). Computed first so we can hand the transcript dir
	// to the executor.
	outboxDir := filepath.Join(cfg.workingDir, ".teem-events")
	if cfg.workingDir == "" {
		outboxDir = filepath.Join(os.TempDir(), "teem-worker-"+cfg.agentID, "events")
	}
	transcriptDir := filepath.Join(outboxDir, "transcripts")

	procExec := executor.NewProcess(transport.LocalTransport{}, jobCwd, nil)
	procExec.AgentID = cfg.agentID
	procExec.TranscriptDir = transcriptDir

	w := &worker{
		agentID:       cfg.agentID,
		role:          cfg.role,
		hostname:      cfg.hostname,
		token:         cfg.token,
		workingDir:    jobCwd,
		exec:          procExec,
		transcriptDir: transcriptDir,
		leaderURL:     cfg.leaderURL,
		jobs:          map[string]*jobRecord{},
		gitOpts:       gitOpts,
		gitAutoPush:   cfg.gitAutoPush && cfg.gitRepoURL != "",
		exitAfterIdle: cfg.exitAfterIdle,
		startedAt:     time.Now(),
		shutdownCh:    make(chan struct{}),
	}

	// Audit outbox: events buffer on disk and drain to the leader when
	// it's reachable. Configured lazily — without a leader URL, events
	// still get written to disk but the sender goroutine is a no-op.
	ob, err := newOutbox(outboxDir, cfg.leaderURL, cfg.token, cfg.agentID, nil)
	if err != nil {
		return fmt.Errorf("outbox: %w", err)
	}
	defer ob.Close()
	w.outbox = ob

	// Shutdown channel: hit POST /shutdown to stop the worker, or
	// arm-and-fire the idle-exit timer. Closed by either path; the
	// signal-context shutdown path picks it up.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", w.handleHealth)
	mux.HandleFunc("/jobs", w.handleJobsCollection)
	mux.HandleFunc("/jobs/", w.handleJob)
	mux.HandleFunc("/shutdown", func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rw.WriteHeader(http.StatusAccepted)
		w.signalShutdown()
	})

	var ln net.Listener
	switch {
	case *unixSocket != "":
		// Clean up a stale socket from a previous run (after a crash
		// or kill). If a worker is genuinely already running we'll
		// fail to listen below, surfacing a clear error.
		_ = os.Remove(*unixSocket)
		if err := os.MkdirAll(filepath.Dir(*unixSocket), 0o755); err != nil {
			return fmt.Errorf("unix socket dir: %w", err)
		}
		ln, err = net.Listen("unix", *unixSocket)
		if err != nil {
			return fmt.Errorf("unix listen: %w", err)
		}
		// On exit, remove the socket so the daemon's reconcile pass
		// doesn't think a dead worker is alive.
		defer os.Remove(*unixSocket)
		fmt.Fprintf(os.Stderr, "[teem-worker] listening on unix:%s\n", *unixSocket)
	case *noTailnet:
		ln, err = net.Listen("tcp", "127.0.0.1"+cfg.listenPort)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		fmt.Fprintf(os.Stderr, "[teem-worker] listening on 127.0.0.1%s (no-tailnet)\n", cfg.listenPort)
	default:
		node, err := tailnet.New(tailnet.Config{
			Hostname:  cfg.hostname,
			AuthKey:   cfg.tsAuthKey,
			Ephemeral: true,
		})
		if err != nil {
			return fmt.Errorf("tailnet: %w", err)
		}
		fmt.Fprintf(os.Stderr, "[teem-worker] joining tailnet as %q...\n", cfg.hostname)
		if err := node.Start(ctx); err != nil {
			return fmt.Errorf("tailnet up: %w", err)
		}
		defer node.Close()
		ln, err = node.Listen("tcp", cfg.listenPort)
		if err != nil {
			return fmt.Errorf("tailnet listen: %w", err)
		}
		fmt.Fprintf(os.Stderr, "[teem-worker] listening on tailnet %s%s\n", cfg.hostname, cfg.listenPort)
		// Replace the outbox's HTTP client with the tailnet-aware one so
		// it can resolve the leader's tailnet hostname.
		ob.client = node.HTTPClient()
	}

	ob.Start(ctx)
	_ = ob.Emit(audit.Event{Kind: "worker_started", Message: cfg.hostname, Meta: map[string]any{"role": cfg.role}})

	// Heartbeat loop — proves liveness when no jobs are in flight, and
	// gives the leader a "last_seen" signal on the registry.
	if cfg.heartbeatInterval > 0 {
		go w.runHeartbeat(ctx, cfg.heartbeatInterval)
	}

	srv := &http.Server{
		Handler:           withAuth(cfg.token, mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-w.shutdownCh:
			fmt.Fprintln(os.Stderr, "[teem-worker] shutdown signal received")
		}
		ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx2)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type config struct {
	agentID    string
	role       string
	hostname   string
	token      string
	tsAuthKey  string
	workingDir string
	listenPort string
	leaderURL  string

	// heartbeatInterval is how often the worker emits a heartbeat
	// audit event. Zero disables heartbeats.
	heartbeatInterval time.Duration

	// exitAfterIdle controls the self-exit timer. After every job
	// completes, if inFlight stays at 0 for this long, the worker
	// emits worker_stopped and shuts itself down. Zero disables.
	// Ephemeral local workers get 2s by default (set by the leader's
	// LocalProvisioner); persistent + remote workers leave it unset.
	exitAfterIdle time.Duration

	// Git settings; cloning happens only when gitRepoURL is non-empty.
	gitRepoURL      string
	gitToken        string
	gitUsername     string
	gitAuthorName   string
	gitAuthorEmail  string
	gitBranchPrefix string
	gitAutoPush     bool
}

func loadConfig(portFlag string) (*config, error) {
	c := &config{
		agentID:           os.Getenv("TEEM_AGENT_ID"),
		role:              os.Getenv("TEEM_AGENT_ROLE"),
		hostname:          os.Getenv("TEEM_WORKER_HOSTNAME"),
		token:             os.Getenv("TEEM_WORKER_TOKEN"),
		tsAuthKey:         os.Getenv("TS_AUTHKEY"),
		workingDir:        os.Getenv("TEEM_WORKER_WORKDIR"),
		leaderURL:         strings.TrimRight(os.Getenv("TEEM_LEADER_URL"), "/"),
		heartbeatInterval: parseHeartbeatInterval(os.Getenv("TEEM_HEARTBEAT_INTERVAL"), 60*time.Second),
		exitAfterIdle:     parseExitAfterIdle(os.Getenv("TEEM_EXIT_AFTER_IDLE")),
		gitRepoURL:        os.Getenv("TEEM_GIT_REPO_URL"),
		gitToken:          os.Getenv("TEEM_GIT_TOKEN"),
		gitUsername:       os.Getenv("TEEM_GIT_USERNAME"),
		gitAuthorName:     os.Getenv("TEEM_GIT_AUTHOR_NAME"),
		gitAuthorEmail:    os.Getenv("TEEM_GIT_AUTHOR_EMAIL"),
		gitBranchPrefix:   os.Getenv("TEEM_GIT_BRANCH_PREFIX"),
		// Default true: ephemeral remote workers lose their work without
		// a push, so unless the operator explicitly opts out we push
		// after every successful job.
		gitAutoPush: parseBool(os.Getenv("TEEM_GIT_AUTO_PUSH"), true),
	}
	if c.agentID == "" {
		c.agentID = "worker"
	}
	if c.hostname == "" {
		c.hostname = "teem-" + c.agentID
	}
	if c.token == "" {
		return nil, fmt.Errorf("TEEM_WORKER_TOKEN is required")
	}
	port := portFlag
	if port == "" {
		port = os.Getenv("TEEM_LISTEN_PORT")
	}
	if port == "" {
		port = ":7780"
	}
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}
	c.listenPort = port
	return c, nil
}

// withAuth gates every route except /healthz behind a shared bearer token.
// /healthz still requires the token — a leader without the token shouldn't
// learn that an agent exists.
func withAuth(token string, h http.Handler) http.Handler {
	expected := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != expected {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

type healthResponse struct {
	OK           bool   `json:"ok"`
	Hostname     string `json:"hostname"`
	AgentID      string `json:"agent_id"`
	Role         string `json:"role,omitempty"`
	JobsInFlight int64  `json:"jobs_in_flight"`
	// State is the worker's lifecycle state ("serving" | "draining" |
	// "shutdown"). Used by the leader's polling debounce to spot a
	// worker that is on its way out before a /jobs POST 409s.
	State string `json:"state,omitempty"`
}

func (w *worker) handleHealth(rw http.ResponseWriter, _ *http.Request) {
	w.mu.Lock()
	st := w.state
	w.mu.Unlock()
	writeJSON(rw, http.StatusOK, healthResponse{
		OK:           true,
		Hostname:     w.hostname,
		AgentID:      w.agentID,
		Role:         w.role,
		JobsInFlight: w.inFlight.Load(),
		State:        st.String(),
	})
}

type jobRequest struct {
	JobID   string        `json:"job_id"`
	Prompt  string        `json:"prompt"`
	Context string        `json:"context,omitempty"`
	MCPs    []team.MCPRef `json:"mcps,omitempty"`
	Skill   string        `json:"skill,omitempty"`
}

type jobResponse struct {
	JobID  string    `json:"job_id"`
	Status jobStatus `json:"status"`
	Output string    `json:"output,omitempty"`
	Error  string    `json:"error,omitempty"`
}

func (w *worker) handleJobsCollection(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req jobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.JobID == "" || req.Prompt == "" {
		http.Error(rw, "job_id and prompt are required", http.StatusBadRequest)
		return
	}

	w.mu.Lock()
	if w.state != stateServing {
		w.mu.Unlock()
		writeJSON(rw, http.StatusConflict, map[string]string{"error": "draining"})
		return
	}
	if _, exists := w.jobs[req.JobID]; exists {
		w.mu.Unlock()
		http.Error(rw, "duplicate job_id", http.StatusConflict)
		return
	}
	rec := newJobRecord()
	w.jobs[req.JobID] = rec
	// Bump inFlight under the same lock as the state check so a
	// concurrent jobEnded() can't observe state=serving && inFlight=0
	// after we've committed to running this job.
	w.inFlight.Add(1)
	w.mu.Unlock()

	go w.runJob(r.Context(), req, rec)

	writeJSON(rw, http.StatusAccepted, jobResponse{JobID: req.JobID, Status: statusPending})
}

func (w *worker) runJob(parent context.Context, req jobRequest, rec *jobRecord) {
	// Use a background context detached from the inbound request so the job
	// outlives the POST. We honor the daemon-wide shutdown signal via parent
	// if available, but the inbound HTTP context dying must not cancel the
	// job.
	_ = parent
	ctx := context.Background()
	// inFlight was incremented by the /jobs handler under w.mu; here we
	// just guarantee the matching decrement and trigger the self-exit
	// check via jobEnded.
	defer w.jobEnded()

	rec.mu.Lock()
	rec.status = statusRunning
	rec.mu.Unlock()

	cap := jobBodyCap()
	_ = w.outbox.Emit(audit.Event{
		JobID: req.JobID,
		Kind:  audit.KindJobReceived,
		Meta: map[string]any{
			"prompt":       truncateString(req.Prompt, cap),
			"prompt_bytes": len(req.Prompt),
			"role":         w.role,
		},
	})

	out, err := w.exec.Execute(ctx, executor.Job{
		ID:      req.JobID,
		Prompt:  req.Prompt,
		Context: req.Context,
		MCPs:    req.MCPs,
		Skill:   req.Skill,
	})
	var errMsg string
	if err != nil {
		errMsg = err.Error()
	}
	rec.finish(out, errMsg)

	if errMsg != "" {
		_ = w.outbox.Emit(audit.Event{
			JobID:   req.JobID,
			Kind:    audit.KindJobError,
			Message: errMsg,
			Meta: map[string]any{
				"output":       truncateString(out, cap),
				"output_bytes": len(out),
			},
		})
		return
	}
	_ = w.outbox.Emit(audit.Event{
		JobID: req.JobID,
		Kind:  audit.KindJobComplete,
		Meta: map[string]any{
			"output":       truncateString(out, cap),
			"output_bytes": len(out),
		},
	})

	// Best-effort: push the full stream-json transcript to the leader
	// and emit a job_transcript_ready event with metadata. Failures are
	// surfaced via audit but never fail the job (the output is already
	// captured in job_complete).
	w.uploadTranscript(ctx, req.JobID, out)

	// Auto-push the agent's branch when configured. Failures are
	// surfaced via audit but don't fail the job — a push retry is
	// always something the operator can do manually.
	if w.gitAutoPush {
		stderr, err := gitsetup.Push(ctx, w.gitOpts)
		if err != nil {
			_ = w.outbox.Emit(audit.Event{
				JobID:   req.JobID,
				Kind:    "git_push_failed",
				Message: err.Error(),
				Meta:    map[string]any{"stderr": stderr},
			})
			return
		}
		_ = w.outbox.Emit(audit.Event{
			JobID: req.JobID,
			Kind:  "git_pushed",
			Meta:  map[string]any{"branch": gitsetup.Branch(w.gitOpts)},
		})
	}
}

func (w *worker) handleJob(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/jobs/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(rw, "bad job id", http.StatusBadRequest)
		return
	}
	w.mu.Lock()
	rec, ok := w.jobs[id]
	w.mu.Unlock()
	if !ok {
		http.Error(rw, "job not found", http.StatusNotFound)
		return
	}

	wait, _ := time.ParseDuration(r.URL.Query().Get("wait"))
	if wait > 0 {
		if wait > 55*time.Second {
			wait = 55 * time.Second
		}
		status, _, _ := rec.snapshot()
		if status == statusPending || status == statusRunning {
			select {
			case <-rec.doneCh:
			case <-time.After(wait):
			case <-r.Context().Done():
			}
		}
	}

	status, output, errMsg := rec.snapshot()
	writeJSON(rw, http.StatusOK, jobResponse{
		JobID:  id,
		Status: status,
		Output: output,
		Error:  errMsg,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// uploadTranscript reads the transcript file the executor produced for
// this job and POSTs the body to the leader's /transcripts/<agent>/<job>
// endpoint. On success it emits job_transcript_ready with a summary
// (first chunk of the final assistant text). Failures surface as a
// transcript_upload_failed event but never fail the job.
func (w *worker) uploadTranscript(ctx context.Context, jobID, finalOutput string) {
	if w.transcriptDir == "" {
		return
	}
	path := filepath.Join(w.transcriptDir, w.agentID, jobID+".jsonl")
	body, err := os.ReadFile(path)
	if err != nil {
		_ = w.outbox.Emit(audit.Event{
			JobID:   jobID,
			Kind:    "transcript_upload_failed",
			Message: "read transcript: " + err.Error(),
			Meta:    map[string]any{"path": path},
		})
		return
	}
	eventCount := bytes.Count(body, []byte{'\n'})

	if w.leaderURL != "" {
		url := w.leaderURL + "/transcripts/" + w.agentID + "/" + jobID
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if rerr == nil {
			req.Header.Set("Authorization", "Bearer "+w.token)
			req.Header.Set("Content-Type", "application/x-ndjson")
			resp, derr := w.outbox.client.Do(req)
			if derr != nil {
				_ = w.outbox.Emit(audit.Event{
					JobID:   jobID,
					Kind:    "transcript_upload_failed",
					Message: derr.Error(),
					Meta:    map[string]any{"path": path, "bytes": len(body)},
				})
				return
			}
			resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				_ = w.outbox.Emit(audit.Event{
					JobID:   jobID,
					Kind:    "transcript_upload_failed",
					Message: "leader returned " + resp.Status,
					Meta:    map[string]any{"path": path, "bytes": len(body)},
				})
				return
			}
		}
	}

	summary := finalOutput
	if len(summary) > 200 {
		summary = summary[:200]
	}
	_ = w.outbox.Emit(audit.Event{
		JobID: jobID,
		Kind:  audit.KindJobTranscriptReady,
		Meta: map[string]any{
			"path":        path,
			"bytes":       len(body),
			"event_count": eventCount,
			"summary":     summary,
		},
	})
}

// runHeartbeat ticks every interval and emits a heartbeat audit event.
// Stops when ctx is cancelled (daemon shutdown). The event meta
// includes in-flight job count and uptime so the leader can spot a
// busy-but-not-progressing worker as well as a stalled one.
func (w *worker) runHeartbeat(ctx context.Context, interval time.Duration) {
	start := time.Now()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_ = w.outbox.Emit(audit.Event{
			Kind: audit.KindHeartbeat,
			Meta: map[string]any{
				"in_flight": w.inFlight.Load(),
				"uptime_s":  int(time.Since(start).Seconds()),
			},
		})
	}
}

// parseHeartbeatInterval reads a Go-duration string from env. "0",
// "off", or "disabled" turn heartbeats off. An invalid value falls back
// to def with a warning to stderr.
func parseHeartbeatInterval(s string, def time.Duration) time.Duration {
	s = strings.TrimSpace(strings.ToLower(s))
	switch s {
	case "":
		return def
	case "0", "off", "disabled", "false", "no":
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		fmt.Fprintf(os.Stderr, "[teem-worker] bad TEEM_HEARTBEAT_INTERVAL %q; using %s\n", s, def)
		return def
	}
	return d
}

// jobBodyCap returns the maximum size of prompt / output strings that
// get embedded in audit events. Reads TEEM_JOB_BODY_CAP_BYTES; default
// 64 KiB. 0 or negative disables truncation (full body is logged).
func jobBodyCap() int {
	v := strings.TrimSpace(os.Getenv("TEEM_JOB_BODY_CAP_BYTES"))
	if v == "" {
		return 64 * 1024
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 64 * 1024
	}
	return n
}

// truncateString clamps s to cap bytes, appending a "<truncated>"
// marker when it actually trimmed something. cap <= 0 disables.
func truncateString(s string, cap int) string {
	if cap <= 0 || len(s) <= cap {
		return s
	}
	return s[:cap] + "\n…<truncated>"
}

// parseExitAfterIdle reads a Go-duration string from env. Empty, "0",
// "off", "disabled", "false", or "no" disables (returns 0). Invalid
// values also return 0 (with a stderr note) — we err on the side of
// staying alive when the operator's config is malformed.
func parseExitAfterIdle(s string) time.Duration {
	s = strings.TrimSpace(strings.ToLower(s))
	switch s {
	case "", "0", "off", "disabled", "false", "no":
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		fmt.Fprintf(os.Stderr, "[teem-worker] bad TEEM_EXIT_AFTER_IDLE %q; staying alive\n", s)
		return 0
	}
	return d
}

// AddPreExitHook registers fn to run inside the self-exit path,
// BEFORE worker_stopped is emitted and the outbox is drained. Each
// hook gets a 10s context; errors are logged but never block exit.
// Hooks run in registration order. Safe to call only before Start.
func (w *worker) AddPreExitHook(fn func(context.Context) error) {
	if fn == nil {
		return
	}
	w.mu.Lock()
	w.preExitHooks = append(w.preExitHooks, fn)
	w.mu.Unlock()
}

// signalShutdown closes shutdownCh exactly once. Used by /shutdown and
// by the self-exit timer's runExitSequence.
func (w *worker) signalShutdown() {
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case <-w.shutdownCh:
		// already closed
	default:
		close(w.shutdownCh)
	}
}

// jobEnded is the bookend for the handler's inFlight.Add(1). It
// decrements inFlight and, if the worker is idle and exitAfterIdle is
// configured, arms the drain timer.
func (w *worker) jobEnded() {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := w.inFlight.Add(-1)
	if w.state != stateServing || w.exitAfterIdle <= 0 || n != 0 {
		return
	}
	w.state = stateDraining
	w.drainTimer = time.AfterFunc(w.exitAfterIdle, w.fireDrain)
}

// fireDrain is the drain timer's callback. Re-checks the state under
// lock (a job may have raced in, though our handler 409s during
// draining — defensive), transitions to shutdown, and runs the exit
// sequence. The lock is released before the (slow) exit sequence
// runs.
func (w *worker) fireDrain() {
	w.mu.Lock()
	if w.state != stateDraining || w.inFlight.Load() != 0 {
		w.mu.Unlock()
		return
	}
	w.state = stateShutdown
	w.mu.Unlock()
	w.runExitSequence()
}

// runExitSequence is the worker-initiated shutdown path:
//
//  1. Run pre-exit hooks (each 10s, errors logged).
//  2. Emit the worker_stopped audit event.
//  3. Block on outbox.Drain (5s) so the leader sees worker_stopped
//     BEFORE the socket closes.
//  4. Close shutdownCh; the server goroutine then calls srv.Shutdown.
//
// Order matters: the leader uses worker_stopped to decide whether to
// skip /shutdown POST during teardown. If the listener went away first
// the leader would have to fall back to the slower liveness watch.
func (w *worker) runExitSequence() {
	// If shutdownCh is already closed, an external trigger (e.g. an
	// operator /shutdown POST) is already winding the worker down —
	// suppress a redundant worker_stopped emit + drain.
	select {
	case <-w.shutdownCh:
		return
	default:
	}
	w.mu.Lock()
	hooks := append([]func(context.Context) error(nil), w.preExitHooks...)
	w.mu.Unlock()
	for _, fn := range hooks {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := fn(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "[teem-worker] pre-exit hook: %v\n", err)
		}
		cancel()
	}

	_ = w.outbox.Emit(audit.Event{
		Kind:    audit.KindWorkerStopped,
		Message: w.hostname,
		Meta: map[string]any{
			"role":     w.role,
			"uptime_s": int(time.Since(w.startedAt).Seconds()),
		},
	})

	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := w.outbox.Drain(drainCtx); err != nil {
		fmt.Fprintf(os.Stderr, "[teem-worker] outbox drain timed out before exit: %v\n", err)
	}
	cancel()

	w.signalShutdown()
}

// parseBool parses a permissive boolean (1/true/yes/on, 0/false/no/off,
// case insensitive). Empty string returns def.
func parseBool(s string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return def
	case "1", "true", "yes", "on", "y", "t":
		return true
	case "0", "false", "no", "off", "n", "f":
		return false
	}
	return def
}
