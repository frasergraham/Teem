package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeWorker mimics the teem-worker daemon's contract closely enough to
// exercise HTTPExecutor's post/long-poll/watchdog logic without depending
// on the real daemon (which would need claude on PATH and tsnet).
type fakeWorker struct {
	t       *testing.T
	token   string
	mu      sync.Mutex
	jobs    map[string]*fakeJob
	healthy bool
}

type fakeJob struct {
	mu     sync.Mutex
	status string
	output string
	err    string
	done   chan struct{}
}

func newFakeWorker(t *testing.T, token string) *fakeWorker {
	return &fakeWorker{t: t, token: token, jobs: map[string]*fakeJob{}, healthy: true}
}

func (f *fakeWorker) setHealthy(v bool) {
	f.mu.Lock()
	f.healthy = v
	f.mu.Unlock()
}

func (f *fakeWorker) finish(jobID, output, errMsg string) {
	f.mu.Lock()
	j := f.jobs[jobID]
	f.mu.Unlock()
	if j == nil {
		f.t.Fatalf("finish: unknown job %s", jobID)
	}
	j.mu.Lock()
	if errMsg != "" {
		j.status = "error"
	} else {
		j.status = "done"
	}
	j.output = output
	j.err = errMsg
	close(j.done)
	j.mu.Unlock()
}

func (f *fakeWorker) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			http.Error(w, "unauth", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		ok := f.healthy
		f.mu.Unlock()
		if !ok {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			http.Error(w, "unauth", http.StatusUnauthorized)
			return
		}
		var req Job
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.jobs[req.ID] = &fakeJob{status: "running", done: make(chan struct{})}
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("/jobs/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			http.Error(w, "unauth", http.StatusUnauthorized)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/jobs/")
		f.mu.Lock()
		j := f.jobs[id]
		f.mu.Unlock()
		if j == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		wait, _ := time.ParseDuration(r.URL.Query().Get("wait"))
		j.mu.Lock()
		status := j.status
		j.mu.Unlock()
		if (status == "pending" || status == "running") && wait > 0 {
			select {
			case <-j.done:
			case <-time.After(wait):
			case <-r.Context().Done():
			}
		}
		j.mu.Lock()
		body := map[string]any{"job_id": id, "status": j.status, "output": j.output, "error": j.err}
		j.mu.Unlock()
		_ = json.NewEncoder(w).Encode(body)
	})
	return mux
}

func TestHTTPExecutor_HappyPath(t *testing.T) {
	worker := newFakeWorker(t, "tok")
	srv := httptest.NewServer(worker.handler())
	defer srv.Close()

	exec := NewHTTP(srv.Client(), srv.URL, "tok")
	exec.PollWait = 200 * time.Millisecond
	exec.HealthCheckEvery = 50 * time.Millisecond

	go func() {
		time.Sleep(100 * time.Millisecond)
		worker.finish("j1", "hello from worker", "")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.Execute(ctx, Job{ID: "j1", Prompt: "hi"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out != "hello from worker" {
		t.Fatalf("output: got %q", out)
	}
}

func TestHTTPExecutor_WorkerErrorIsSurfaced(t *testing.T) {
	worker := newFakeWorker(t, "tok")
	srv := httptest.NewServer(worker.handler())
	defer srv.Close()

	exec := NewHTTP(srv.Client(), srv.URL, "tok")
	exec.PollWait = 100 * time.Millisecond

	go func() {
		time.Sleep(50 * time.Millisecond)
		worker.finish("j2", "", "claude exit 1")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := exec.Execute(ctx, Job{ID: "j2", Prompt: "hi"})
	if err == nil || !strings.Contains(err.Error(), "claude exit 1") {
		t.Fatalf("want claude exit 1 error, got %v", err)
	}
}

func TestHTTPExecutor_WatchdogFiresWhenWorkerDies(t *testing.T) {
	worker := newFakeWorker(t, "tok")
	srv := httptest.NewServer(worker.handler())
	defer srv.Close()

	exec := NewHTTP(srv.Client(), srv.URL, "tok")
	exec.PollWait = 500 * time.Millisecond
	exec.HealthCheckEvery = 30 * time.Millisecond
	exec.MaxHealthFailures = 2

	// Job never finishes; flip /healthz to 503 right after submit.
	go func() {
		time.Sleep(50 * time.Millisecond)
		worker.setHealthy(false)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := exec.Execute(ctx, Job{ID: "j3", Prompt: "hi"})
	if err == nil || !strings.Contains(err.Error(), "worker unreachable") {
		t.Fatalf("want worker unreachable, got %v", err)
	}
}

func TestHTTPExecutor_AuthRequired(t *testing.T) {
	worker := newFakeWorker(t, "right-token")
	srv := httptest.NewServer(worker.handler())
	defer srv.Close()

	exec := NewHTTP(srv.Client(), srv.URL, "wrong-token")
	exec.PollWait = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := exec.Execute(ctx, Job{ID: "j4", Prompt: "hi"})
	if err == nil {
		t.Fatalf("expected auth error")
	}
}
