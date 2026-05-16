package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/team"
)

// newTestTeam builds a minimal registeredTeam suitable for exercising
// the SSR routes. auditSink is a fresh FileSink in tmp; transcriptsDir
// is a fresh dir; registry is empty unless the caller adds entries.
func newTestTeam(t *testing.T, name string) *registeredTeam {
	t.Helper()
	tmp := t.TempDir()
	auditPath := filepath.Join(tmp, "audit.jsonl")
	sink, err := audit.OpenFile(auditPath)
	if err != nil {
		t.Fatalf("audit open: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	return &registeredTeam{
		// id == name for the test so URL assertions like
		// `/teams/alpha/...` keep matching.
		team:           &team.Team{ID: name, Name: name},
		auditSink:      sink,
		registry:       mcpsrv.NewRegistry(),
		transcriptsDir: filepath.Join(tmp, "transcripts"),
		registered:     time.Now().Add(-1 * time.Hour),
	}
}

func writeAudit(t *testing.T, sink audit.Sink, events ...audit.Event) {
	t.Helper()
	for _, e := range events {
		if err := sink.Write(e); err != nil {
			t.Fatalf("audit write: %v", err)
		}
	}
}

func TestRenderAgentJobs_RendersJobRows(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	t0 := time.Now().Add(-5 * time.Minute)
	writeAudit(t, rt.auditSink,
		audit.Event{Timestamp: t0, AgentID: "worker-1", JobID: "j-aaa", Kind: audit.KindJobReceived,
			Meta: map[string]any{"prompt": "first prompt"}},
		audit.Event{Timestamp: t0.Add(2 * time.Second), AgentID: "worker-1", JobID: "j-aaa", Kind: audit.KindJobComplete,
			Meta: map[string]any{"output": "first output"}},
		audit.Event{Timestamp: t0.Add(3 * time.Second), AgentID: "worker-1", JobID: "j-aaa", Kind: audit.KindJobTranscriptReady,
			Meta: map[string]any{"path": "/tmp/x.jsonl", "bytes": 42, "summary": "wrote a fix"}},

		audit.Event{Timestamp: t0.Add(1 * time.Minute), AgentID: "worker-1", JobID: "j-bbb", Kind: audit.KindJobReceived,
			Meta: map[string]any{"prompt": "second prompt"}},
		audit.Event{Timestamp: t0.Add(2 * time.Minute), AgentID: "worker-1", JobID: "j-bbb", Kind: audit.KindJobError,
			Message: "claude exit 1"},

		// One job belonging to a different agent — must not appear.
		audit.Event{Timestamp: t0.Add(10 * time.Second), AgentID: "worker-2", JobID: "j-zzz", Kind: audit.KindJobReceived,
			Meta: map[string]any{"prompt": "other agent"}},
	)

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/agents/worker-1/jobs", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"worker-1",
		"j-aaa", "j-bbb",
		"done", "error",
		"wrote a fix",
		`href="/teams/alpha/jobs/j-aaa"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body", want)
		}
	}
	if strings.Contains(body, "j-zzz") {
		t.Errorf("body leaked another agent's job: %s", body)
	}
}

func TestRenderAgentJobs_UnknownTeam(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	req := httptest.NewRequest(http.MethodGet, "/teams/no-such/agents/worker-1/jobs", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestRenderJobDetail_RendersTurns(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	t0 := time.Now().Add(-2 * time.Minute)
	writeAudit(t, rt.auditSink,
		audit.Event{Timestamp: t0, AgentID: "worker-1", JobID: "j-aaa", Kind: audit.KindJobReceived,
			Meta: map[string]any{"prompt": "<script>alert(1)</script>"}},
		audit.Event{Timestamp: t0.Add(1 * time.Second), AgentID: "worker-1", JobID: "j-aaa", Kind: audit.KindJobComplete,
			Meta: map[string]any{"output": "done"}},
	)

	// Write a small transcript.
	dir := filepath.Join(rt.transcriptsDir, "worker-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	transcript := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello <world>"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","text":"file.txt"}]}}`,
		`{"type":"result","result":"final answer"}`,
		"",
	}, "\n")
	tfile := filepath.Join(dir, "j-aaa.jsonl")
	if err := os.WriteFile(tfile, []byte(transcript), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/jobs/j-aaa", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"j-aaa",
		"worker-1",
		"hello &lt;world&gt;", // assistant text, html-escaped
		"Tool call · Bash",    // tool_use card title
		"file.txt",            // tool_result body
		"final answer",        // result event
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body", want)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Errorf("html escape failed — raw <script> in body")
	}
}

func TestRenderJobDetail_TranscriptMissing(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	t0 := time.Now().Add(-2 * time.Minute)
	writeAudit(t, rt.auditSink,
		audit.Event{Timestamp: t0, AgentID: "worker-1", JobID: "j-aaa", Kind: audit.KindJobReceived,
			Meta: map[string]any{"prompt": "only audit, no transcript"}},
		audit.Event{Timestamp: t0.Add(1 * time.Second), AgentID: "worker-1", JobID: "j-aaa", Kind: audit.KindJobComplete,
			Meta: map[string]any{"output": "captured output"}},
	)

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/jobs/j-aaa", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"only audit, no transcript", "captured output"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q (fallback rendering) in body", want)
		}
	}
}

func TestDashboardLinksToJobsPages(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newTestTeam(t, "alpha")
	rt.registry.Add(mcpsrv.AgentEntry{
		ID:    "worker-1",
		Role:  "implementer",
		State: mcpsrv.StateRunning,
	})
	writeAudit(t, rt.auditSink, audit.Event{
		Timestamp: time.Now(),
		AgentID:   "worker-1",
		JobID:     "j-aaa",
		Kind:      audit.KindJobComplete,
		Meta:      map[string]any{"output": "ok"},
	})
	d.teams["alpha"] = rt

	// Per-agent jobs URL and per-job detail URL only render in the
	// per-team detail page; the summary index is counters-only.
	req := httptest.NewRequest(http.MethodGet, "/teams/alpha", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/teams/alpha/agents/worker-1/jobs"`) {
		t.Errorf("agent id is not linked to its jobs page: %s", body)
	}
	if !strings.Contains(body, `href="/teams/alpha/jobs/j-aaa"`) {
		t.Errorf("recent event kind is not linked to its job detail: %s", body)
	}
}

func TestResolveAgentJobsRoute(t *testing.T) {
	cases := []struct {
		in     string
		wantID string
		wantOK bool
	}{
		{"/agents/worker-1/jobs", "worker-1", true},
		{"/agents/worker-1.foo/jobs", "worker-1.foo", true},
		{"/agents//jobs", "", false},
		{"/agents/worker-1", "", false},
		{"/jobs/worker-1", "", false},
	}
	for _, tc := range cases {
		got, ok := resolveAgentJobsRoute(tc.in)
		if got != tc.wantID || ok != tc.wantOK {
			t.Errorf("%q: got (%q,%v), want (%q,%v)", tc.in, got, ok, tc.wantID, tc.wantOK)
		}
	}
}
