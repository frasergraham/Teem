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

type worker struct {
	agentID    string
	role       string
	hostname   string
	token      string
	workingDir string
	exec       executor.Executor
	outbox     *outbox

	// Git auto-push wiring; populated when TEEM_GIT_REPO_URL was set at
	// startup. gitAutoPush is the resolved boolean (true only when both
	// the repo is configured and auto-push isn't disabled by the
	// operator).
	gitOpts     gitsetup.Options
	gitAutoPush bool

	mu      sync.Mutex
	jobs    map[string]*jobRecord
	inFlight atomic.Int64
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
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
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

	w := &worker{
		agentID:    cfg.agentID,
		role:       cfg.role,
		hostname:   cfg.hostname,
		token:      cfg.token,
		workingDir: jobCwd,
		exec:       executor.NewProcess(transport.LocalTransport{}, jobCwd, nil),
		jobs:       map[string]*jobRecord{},
		gitOpts:    gitOpts,
		gitAutoPush: cfg.gitAutoPush && cfg.gitRepoURL != "",
	}

	// Audit outbox: events buffer on disk and drain to the leader when
	// it's reachable. Configured lazily — without a leader URL, events
	// still get written to disk but the sender goroutine is a no-op.
	outboxDir := filepath.Join(cfg.workingDir, ".teem-events")
	if cfg.workingDir == "" {
		outboxDir = filepath.Join(os.TempDir(), "teem-worker-"+cfg.agentID, "events")
	}
	ob, err := newOutbox(outboxDir, cfg.leaderURL, cfg.token, cfg.agentID, nil)
	if err != nil {
		return fmt.Errorf("outbox: %w", err)
	}
	defer ob.Close()
	w.outbox = ob

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", w.handleHealth)
	mux.HandleFunc("/jobs", w.handleJobsCollection)
	mux.HandleFunc("/jobs/", w.handleJob)

	var ln net.Listener
	if *noTailnet {
		ln, err = net.Listen("tcp", "127.0.0.1"+cfg.listenPort)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		fmt.Fprintf(os.Stderr, "[teem-worker] listening on 127.0.0.1%s (no-tailnet)\n", cfg.listenPort)
	} else {
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

	srv := &http.Server{
		Handler:           withAuth(cfg.token, mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
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
		agentID:         os.Getenv("TEEM_AGENT_ID"),
		role:            os.Getenv("TEEM_AGENT_ROLE"),
		hostname:        os.Getenv("TEEM_WORKER_HOSTNAME"),
		token:           os.Getenv("TEEM_WORKER_TOKEN"),
		tsAuthKey:       os.Getenv("TS_AUTHKEY"),
		workingDir:      os.Getenv("TEEM_WORKER_WORKDIR"),
		leaderURL:       strings.TrimRight(os.Getenv("TEEM_LEADER_URL"), "/"),
		gitRepoURL:      os.Getenv("TEEM_GIT_REPO_URL"),
		gitToken:        os.Getenv("TEEM_GIT_TOKEN"),
		gitUsername:     os.Getenv("TEEM_GIT_USERNAME"),
		gitAuthorName:   os.Getenv("TEEM_GIT_AUTHOR_NAME"),
		gitAuthorEmail:  os.Getenv("TEEM_GIT_AUTHOR_EMAIL"),
		gitBranchPrefix: os.Getenv("TEEM_GIT_BRANCH_PREFIX"),
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
	OK             bool   `json:"ok"`
	Hostname       string `json:"hostname"`
	AgentID        string `json:"agent_id"`
	Role           string `json:"role,omitempty"`
	JobsInFlight   int64  `json:"jobs_in_flight"`
}

func (w *worker) handleHealth(rw http.ResponseWriter, _ *http.Request) {
	writeJSON(rw, http.StatusOK, healthResponse{
		OK:           true,
		Hostname:     w.hostname,
		AgentID:      w.agentID,
		Role:         w.role,
		JobsInFlight: w.inFlight.Load(),
	})
}

type jobRequest struct {
	JobID   string         `json:"job_id"`
	Prompt  string         `json:"prompt"`
	Context string         `json:"context,omitempty"`
	MCPs    []team.MCPRef  `json:"mcps,omitempty"`
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
	if _, exists := w.jobs[req.JobID]; exists {
		w.mu.Unlock()
		http.Error(rw, "duplicate job_id", http.StatusConflict)
		return
	}
	rec := newJobRecord()
	w.jobs[req.JobID] = rec
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
	w.inFlight.Add(1)
	defer w.inFlight.Add(-1)

	rec.mu.Lock()
	rec.status = statusRunning
	rec.mu.Unlock()

	_ = w.outbox.Emit(audit.Event{JobID: req.JobID, Kind: audit.KindJobReceived})

	out, err := w.exec.Execute(ctx, executor.Job{
		ID:      req.JobID,
		Prompt:  req.Prompt,
		Context: req.Context,
		MCPs:    req.MCPs,
	})
	var errMsg string
	if err != nil {
		errMsg = err.Error()
	}
	rec.finish(out, errMsg)

	if errMsg != "" {
		_ = w.outbox.Emit(audit.Event{JobID: req.JobID, Kind: audit.KindJobError, Message: errMsg})
		return
	}
	_ = w.outbox.Emit(audit.Event{JobID: req.JobID, Kind: audit.KindJobComplete, Meta: map[string]any{"output_bytes": len(out)}})

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
