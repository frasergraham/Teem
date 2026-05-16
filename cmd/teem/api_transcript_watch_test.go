package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
)

// withFastWatchTimers shrinks the watch handler's poll and keepalive
// intervals so tests don't have to wait 300ms / 15s for behaviour.
// Restored on Cleanup.
func withFastWatchTimers(t *testing.T) {
	t.Helper()
	origPoll := transcriptWatchPollInterval
	origKA := transcriptWatchKeepalive
	transcriptWatchPollInterval = 10 * time.Millisecond
	transcriptWatchKeepalive = 50 * time.Millisecond
	t.Cleanup(func() {
		transcriptWatchPollInterval = origPoll
		transcriptWatchKeepalive = origKA
	})
}

// TestAPITeamTranscriptWatch_StreamsExistingThenNew asserts the open-
// state behaviour: existing lines flush immediately, lines appended
// while the stream is live show up as SSE `data:` events, and a
// `result` line closes the stream with `event: done`.
func TestAPITeamTranscriptWatch_StreamsExistingThenNew(t *testing.T) {
	withFastWatchTimers(t)
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	dir := filepath.Join(rt.transcriptsDir, "worker-ben")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "j-1.jsonl")
	// Two existing lines before the handler is hit.
	initial := `{"type":"system","subtype":"init"}` + "\n" +
		`{"type":"assistant","message":"hello"}` + "\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/teams/alpha/transcripts/worker-ben/j-1/watch", nil)
	w := httptest.NewRecorder()

	// Run the handler in a goroutine; append two more lines after
	// it's had a chance to drain the initial bytes.
	doneCh := make(chan struct{})
	go func() {
		d.handler().ServeHTTP(w, req)
		close(doneCh)
	}()

	// Wait long enough for the first pump to flush the existing
	// lines and re-enter the poll select. Then append a fresh
	// assistant turn and a `result` line to terminate the stream.
	time.Sleep(50 * time.Millisecond)
	appendFile(t, path, `{"type":"assistant","message":"world"}`+"\n")
	time.Sleep(50 * time.Millisecond)
	appendFile(t, path, `{"type":"result","subtype":"success"}`+"\n")

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("watch handler did not return within 2s")
	}

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type=%q want text/event-stream", ct)
	}
	body := w.Body.String()
	for _, want := range []string{
		`data: {"type":"system","subtype":"init"}`,
		`data: {"type":"assistant","message":"hello"}`,
		`data: {"type":"assistant","message":"world"}`,
		`data: {"type":"result","subtype":"success"}`,
		"event: done",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody=%s", want, body)
		}
	}
}

// TestAPITeamTranscriptWatch_TerminatesOnResult checks the simpler
// case where the file already contains the full transcript including
// a `result` line — the handler must drain it, emit `done`, and
// return without polling.
func TestAPITeamTranscriptWatch_TerminatesOnResult(t *testing.T) {
	withFastWatchTimers(t)
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	dir := filepath.Join(rt.transcriptsDir, "worker-ben")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "j-2.jsonl")
	body := `{"type":"system","subtype":"init"}` + "\n" +
		`{"type":"assistant","message":"hi"}` + "\n" +
		`{"type":"result","subtype":"success"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/teams/alpha/transcripts/worker-ben/j-2/watch", nil)
	w := httptest.NewRecorder()

	doneCh := make(chan struct{})
	go func() {
		d.handler().ServeHTTP(w, req)
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after result line")
	}

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	got := w.Body.String()
	if !strings.Contains(got, "event: done") {
		t.Errorf("missing event: done\nbody=%s", got)
	}
	// All three original lines must be present.
	for _, want := range []string{
		`data: {"type":"system","subtype":"init"}`,
		`data: {"type":"assistant","message":"hi"}`,
		`data: {"type":"result","subtype":"success"}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing %q\nbody=%s", want, got)
		}
	}
}

// TestAPITeamTranscriptWatch_MissingFile_404 — before the stream
// opens we expect a clean 404. The SPA disables the Watch button when
// current_job_id is empty so this is a defensive check.
func TestAPITeamTranscriptWatch_MissingFile_404(t *testing.T) {
	withFastWatchTimers(t)
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/api/teams/alpha/transcripts/worker-ben/no-such-job/watch", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404 body=%s", w.Code, w.Body.String())
	}
}

// TestWorkerRow_PopulatesCurrentJobID drives the currentJobsByAgent
// helper against an audit sink: agent A has an in-flight job (no
// terminal event), agent B has a completed job, agent C started two
// jobs and finished the first one. Expectations:
//   - A → job-a1
//   - B → ""        (completed; not in the map)
//   - C → job-c2    (latest job_received, still in flight)
func TestWorkerRow_PopulatesCurrentJobID(t *testing.T) {
	rt := newFullTestTeam(t, "alpha")
	now := time.Now().UTC()

	mustWrite(t, rt.auditSink, audit.Event{
		Timestamp: now.Add(-10 * time.Minute),
		AgentID:   "worker-a", JobID: "job-a1", Kind: audit.KindJobReceived,
	})
	// agent B: started + completed
	mustWrite(t, rt.auditSink, audit.Event{
		Timestamp: now.Add(-15 * time.Minute),
		AgentID:   "worker-b", JobID: "job-b1", Kind: audit.KindJobReceived,
	})
	mustWrite(t, rt.auditSink, audit.Event{
		Timestamp: now.Add(-14 * time.Minute),
		AgentID:   "worker-b", JobID: "job-b1", Kind: audit.KindJobComplete,
	})
	// agent C: two starts, only the older terminates
	mustWrite(t, rt.auditSink, audit.Event{
		Timestamp: now.Add(-9 * time.Minute),
		AgentID:   "worker-c", JobID: "job-c1", Kind: audit.KindJobReceived,
	})
	mustWrite(t, rt.auditSink, audit.Event{
		Timestamp: now.Add(-8 * time.Minute),
		AgentID:   "worker-c", JobID: "job-c1", Kind: audit.KindJobComplete,
	})
	mustWrite(t, rt.auditSink, audit.Event{
		Timestamp: now.Add(-1 * time.Minute),
		AgentID:   "worker-c", JobID: "job-c2", Kind: audit.KindJobReceived,
	})

	got := currentJobsByAgent(rt)
	if got["worker-a"] != "job-a1" {
		t.Errorf("worker-a current = %q want job-a1", got["worker-a"])
	}
	if _, ok := got["worker-b"]; ok {
		t.Errorf("worker-b should be absent; got %q", got["worker-b"])
	}
	if got["worker-c"] != "job-c2" {
		t.Errorf("worker-c current = %q want job-c2", got["worker-c"])
	}
}

// TestSplitWatchPath exercises the rest-string parsing the dispatcher
// uses to decide between the watch handler and the existing read.
func TestSplitWatchPath(t *testing.T) {
	cases := []struct {
		in   string
		a, j string
		isOK bool
	}{
		{"ada/j-1/watch", "ada", "j-1", true},
		{"ada/j-1", "", "", false},             // no /watch suffix → read endpoint
		{"ada/j-1/", "", "", false},            // trailing slash, no segment
		{"ada/j-1/watch/extra", "", "", false}, // four segments
		{"ada//watch", "", "", false},          // empty job
		{"/j-1/watch", "", "", false},          // empty agent
	}
	for _, c := range cases {
		a, j, ok := splitWatchPath(c.in)
		if ok != c.isOK || a != c.a || j != c.j {
			t.Errorf("splitWatchPath(%q) = (%q,%q,%t), want (%q,%q,%t)",
				c.in, a, j, ok, c.a, c.j, c.isOK)
		}
	}
}

func appendFile(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, sink audit.Sink, e audit.Event) {
	t.Helper()
	if err := sink.Write(e); err != nil {
		t.Fatal(err)
	}
}
