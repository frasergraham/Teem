package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
)

// newTestWorker stands up a worker backed by an on-disk outbox in a
// temp dir, with no leader URL (so the outbox is write-only). Tests
// can read outbox.jsonl directly to assert which events were emitted.
func newTestWorker(t *testing.T, exitAfterIdle time.Duration) *worker {
	t.Helper()
	dir := t.TempDir()
	ob, err := newOutbox(dir, "", "tok", "test-1", nil)
	if err != nil {
		t.Fatalf("newOutbox: %v", err)
	}
	t.Cleanup(func() { _ = ob.Close() })
	return &worker{
		agentID:       "test-1",
		role:          "tester",
		hostname:      "teem-test-1",
		token:         "tok",
		outbox:        ob,
		jobs:          map[string]*jobRecord{},
		exitAfterIdle: exitAfterIdle,
		startedAt:     time.Now(),
		shutdownCh:    make(chan struct{}),
	}
}

func TestParseExitAfterIdle(t *testing.T) {
	cases := map[string]time.Duration{
		"":         0,
		"0":        0,
		"off":      0,
		"disabled": 0,
		"false":    0,
		"no":       0,
		"NotADur":  0, // invalid → 0 (with stderr warning)
		"2s":       2 * time.Second,
		"500ms":    500 * time.Millisecond,
		"-1s":      0, // negative → 0
		" 2s ":     2 * time.Second,
		" OFF ":    0,
	}
	for in, want := range cases {
		got := parseExitAfterIdle(in)
		if got != want {
			t.Errorf("parseExitAfterIdle(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestRejectsJobWhenDraining verifies the /jobs POST handler returns
// 409 with body {"error":"draining"} whenever state != serving.
func TestRejectsJobWhenDraining(t *testing.T) {
	w := newTestWorker(t, 100*time.Millisecond)
	w.state = stateDraining

	body, _ := json.Marshal(jobRequest{JobID: "j1", Prompt: "p"})
	req := httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewReader(body))
	rw := httptest.NewRecorder()
	w.handleJobsCollection(rw, req)

	if rw.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rw.Code, rw.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["error"] != "draining" {
		t.Errorf("body = %v, want error=draining", got)
	}
}

// TestRejectsJobWhenShutdown is the same check for the terminal state
// — we never accept a job once the exit sequence has started.
func TestRejectsJobWhenShutdown(t *testing.T) {
	w := newTestWorker(t, 100*time.Millisecond)
	w.state = stateShutdown

	body, _ := json.Marshal(jobRequest{JobID: "j1", Prompt: "p"})
	req := httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewReader(body))
	rw := httptest.NewRecorder()
	w.handleJobsCollection(rw, req)

	if rw.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rw.Code)
	}
}

// TestHealthExposesState confirms /healthz reports the worker's
// current lifecycle state — the leader uses this for debounce.
func TestHealthExposesState(t *testing.T) {
	w := newTestWorker(t, 100*time.Millisecond)

	for _, st := range []workerState{stateServing, stateDraining, stateShutdown} {
		w.mu.Lock()
		w.state = st
		w.mu.Unlock()

		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rw := httptest.NewRecorder()
		w.handleHealth(rw, req)
		var got healthResponse
		if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.State != st.String() {
			t.Errorf("/healthz state = %q, want %q", got.State, st.String())
		}
	}
}

// TestJobEndedArmsDrain checks that completing the only in-flight
// job transitions to draining and schedules a timer. We use a tiny
// exitAfterIdle to keep the test fast.
func TestJobEndedArmsDrain(t *testing.T) {
	w := newTestWorker(t, 50*time.Millisecond)
	// Simulate a job that's been accepted: state=serving, inFlight=1.
	w.inFlight.Add(1)
	w.jobEnded()

	w.mu.Lock()
	gotState := w.state
	armed := w.drainTimer != nil
	w.mu.Unlock()
	if gotState != stateDraining {
		t.Errorf("state = %v, want draining", gotState)
	}
	if !armed {
		t.Error("drainTimer was not armed")
	}
}

// TestExitAfterIdleDisabledStaysServing ensures a worker with no exit
// window stays in stateServing forever — this protects persistent +
// remote workers where TEEM_EXIT_AFTER_IDLE is unset.
func TestExitAfterIdleDisabledStaysServing(t *testing.T) {
	w := newTestWorker(t, 0)
	w.inFlight.Add(1)
	w.jobEnded()

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state != stateServing {
		t.Errorf("state = %v, want serving (exit-after-idle disabled)", w.state)
	}
	if w.drainTimer != nil {
		t.Error("drainTimer was armed; expected disabled")
	}
}

// TestFireDrainEmitsWorkerStoppedAndShutsDown is the happy-path
// end-to-end: complete the only job, wait for the drain timer to
// fire, verify worker_stopped is on disk and shutdownCh is closed.
// We also check pre-exit hooks ran before the worker_stopped event.
func TestFireDrainEmitsWorkerStoppedAndShutsDown(t *testing.T) {
	w := newTestWorker(t, 25*time.Millisecond)

	sentinelWritten := make(chan time.Time, 1)
	w.AddPreExitHook(func(ctx context.Context) error {
		sentinelWritten <- time.Now().UTC()
		return nil
	})

	w.inFlight.Add(1)
	w.jobEnded()

	// Wait for the timer + exit sequence to run.
	select {
	case <-w.shutdownCh:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdownCh was never closed")
	}

	// Pre-exit hook fired.
	var hookTime time.Time
	select {
	case hookTime = <-sentinelWritten:
	default:
		t.Fatal("pre-exit hook did not run")
	}

	// Pull events from the outbox file.
	events := readOutboxEvents(t, w.outbox.dir)
	var stopEvent *audit.Event
	for i := range events {
		if events[i].Kind == audit.KindWorkerStopped {
			stopEvent = &events[i]
			break
		}
	}
	if stopEvent == nil {
		t.Fatalf("worker_stopped not emitted; events: %+v", events)
	}
	if stopEvent.AgentID != "test-1" {
		t.Errorf("worker_stopped.agent_id = %q", stopEvent.AgentID)
	}
	if got, _ := stopEvent.Meta["role"].(string); got != "tester" {
		t.Errorf("worker_stopped.meta.role = %q", got)
	}
	// Hook must fire before the audit event (the spec calls this out
	// because hooks may want to emit final events themselves).
	if !hookTime.Before(stopEvent.Timestamp) && !hookTime.Equal(stopEvent.Timestamp) {
		t.Errorf("pre-exit hook ran at %s, after worker_stopped at %s", hookTime, stopEvent.Timestamp)
	}

	// Final state is shutdown.
	w.mu.Lock()
	st := w.state
	w.mu.Unlock()
	if st != stateShutdown {
		t.Errorf("final state = %v, want shutdown", st)
	}
}

// readOutboxEvents tails the outbox.jsonl and decodes everything to
// audit.Events. Test helper only.
func readOutboxEvents(t *testing.T, dir string) []audit.Event {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "outbox.jsonl"))
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	var out []audit.Event
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e audit.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}
