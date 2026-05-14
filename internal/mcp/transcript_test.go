package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/team"
)

const sampleTranscript = `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"thinking…"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{}}]}}
{"type":"result","result":"done"}
`

func newTestServerWithTranscripts(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	tm := &team.Team{
		Name:       "t",
		Leader:     team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{{Role: "worker", Placement: "local", MaxConcurrent: 1}},
	}
	srv, err := New(Config{
		Bus:            bus.NewMemBus(),
		Team:           tm,
		Registry:       NewRegistry(),
		Spawner:        &fakeSpawner{running: map[string]bool{}, roles: map[string]string{}},
		TranscriptsDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, dir
}

func writeSampleTranscript(t *testing.T, dir, agent, job string) {
	t.Helper()
	sub := filepath.Join(dir, agent)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, job+".jsonl"), []byte(sampleTranscript), 0o600); err != nil {
		t.Fatal(err)
	}
}

func callGetJobTranscript(t *testing.T, srv *Server, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "get_job_transcript"
	req.Params.Arguments = args
	res, err := srv.handleGetJobTranscript(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	return res
}

func TestGetJobTranscript_RawHappyPath(t *testing.T) {
	srv, dir := newTestServerWithTranscripts(t)
	writeSampleTranscript(t, dir, "wk-1", "j1")

	res := callGetJobTranscript(t, srv, map[string]any{
		"agent_id": "wk-1",
		"job_id":   "j1",
		"format":   "raw",
	})
	if res.IsError {
		t.Fatalf("error: %s", resultText(t, res))
	}
	var resp struct {
		AgentID     string `json:"agent_id"`
		JobID       string `json:"job_id"`
		Format      string `json:"format"`
		TotalBytes  int    `json:"total_bytes"`
		TotalEvents int    `json:"total_events"`
		Truncated   bool   `json:"truncated"`
		Content     string `json:"content"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TotalEvents != 4 {
		t.Errorf("total_events: got %d want 4", resp.TotalEvents)
	}
	if resp.Format != "raw" {
		t.Errorf("format: %q", resp.Format)
	}
	if resp.Content != sampleTranscript {
		t.Errorf("raw content not verbatim:\n got:  %q\n want: %q", resp.Content, sampleTranscript)
	}
}

func TestGetJobTranscript_TextRendering(t *testing.T) {
	srv, dir := newTestServerWithTranscripts(t)
	writeSampleTranscript(t, dir, "wk-1", "j1")

	res := callGetJobTranscript(t, srv, map[string]any{
		"agent_id": "wk-1",
		"job_id":   "j1",
	})
	if res.IsError {
		t.Fatalf("error: %s", resultText(t, res))
	}
	var resp struct {
		Content string `json:"content"`
		Format  string `json:"format"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Format != "text" {
		t.Errorf("default format should be 'text', got %q", resp.Format)
	}
	// Expect to see the assistant text + tool_use + result lines.
	for _, want := range []string{"[assistant] thinking", "[assistant tool_use] Read", "[result] done"} {
		if !strings.Contains(resp.Content, want) {
			t.Errorf("text rendering missing %q in:\n%s", want, resp.Content)
		}
	}
}

func TestGetJobTranscript_HeadLimitsLines(t *testing.T) {
	srv, dir := newTestServerWithTranscripts(t)
	writeSampleTranscript(t, dir, "wk-1", "j1")

	res := callGetJobTranscript(t, srv, map[string]any{
		"agent_id": "wk-1",
		"job_id":   "j1",
		"format":   "raw",
		"head":     "2",
	})
	if res.IsError {
		t.Fatalf("error: %s", resultText(t, res))
	}
	var resp struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal([]byte(resultText(t, res)), &resp)
	gotLines := strings.Count(resp.Content, "\n")
	if gotLines != 2 {
		t.Errorf("head=2: got %d lines, want 2:\n%s", gotLines, resp.Content)
	}
}

func TestGetJobTranscript_404OnMissing(t *testing.T) {
	srv, _ := newTestServerWithTranscripts(t)
	res := callGetJobTranscript(t, srv, map[string]any{
		"agent_id": "ghost",
		"job_id":   "j9",
	})
	if !res.IsError {
		t.Fatalf("want error for missing transcript, got: %s", resultText(t, res))
	}
}

func TestGetJobTranscript_RejectsBadIDs(t *testing.T) {
	srv, _ := newTestServerWithTranscripts(t)
	res := callGetJobTranscript(t, srv, map[string]any{
		"agent_id": "../escape",
		"job_id":   "j1",
	})
	if !res.IsError {
		t.Fatal("expected error for path-traversal agent_id")
	}
}
