package inflight

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func openTest(t *testing.T) (*Log, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "in-flight.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return l, path
}

func TestInflight_StartEndPair(t *testing.T) {
	l, _ := openTest(t)
	defer l.Close()

	if err := l.RecordStart("j1", "worker-1", "do thing"); err != nil {
		t.Fatal(err)
	}
	out, err := l.Outstanding()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].JobID != "j1" || out[0].AgentID != "worker-1" {
		t.Errorf("after start: %+v", out)
	}
	if out[0].PromptPreview != "do thing" {
		t.Errorf("preview missing: %q", out[0].PromptPreview)
	}

	if err := l.RecordEnd("j1"); err != nil {
		t.Fatal(err)
	}
	out, _ = l.Outstanding()
	if len(out) != 0 {
		t.Errorf("after end, expected 0 outstanding, got %d", len(out))
	}
}

func TestInflight_OutstandingAfterReopen(t *testing.T) {
	l, path := openTest(t)
	_ = l.RecordStart("j1", "worker-1", "first")
	_ = l.RecordStart("j2", "worker-2", "second")
	_ = l.RecordEnd("j1")
	_ = l.Close()

	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	out, _ := l2.Outstanding()
	if len(out) != 1 || out[0].JobID != "j2" {
		t.Errorf("after reopen, outstanding = %+v, want only j2", out)
	}
}

func TestInflight_Reset(t *testing.T) {
	l, path := openTest(t)
	defer l.Close()
	_ = l.RecordStart("j1", "worker-1", "x")
	_ = l.RecordStart("j2", "worker-2", "y")

	if err := l.Reset(); err != nil {
		t.Fatal(err)
	}
	out, _ := l.Outstanding()
	if len(out) != 0 {
		t.Errorf("after Reset, expected empty, got %d", len(out))
	}
	// File should still exist (just truncated).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("Reset should truncate; size = %d", info.Size())
	}

	// After Reset, new writes work.
	if err := l.RecordStart("j3", "worker-3", "fresh"); err != nil {
		t.Fatal(err)
	}
	out, _ = l.Outstanding()
	if len(out) != 1 || out[0].JobID != "j3" {
		t.Errorf("post-Reset write: %+v", out)
	}
}

func TestInflight_MultipleOutstanding_Stable(t *testing.T) {
	l, _ := openTest(t)
	defer l.Close()
	for _, id := range []string{"j1", "j2", "j3"} {
		_ = l.RecordStart(id, "worker-1", id)
	}
	_ = l.RecordEnd("j2")
	out, _ := l.Outstanding()
	ids := []string{}
	for _, r := range out {
		ids = append(ids, r.JobID)
	}
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "j1" || ids[1] != "j3" {
		t.Errorf("outstanding ids: %v", ids)
	}
}

func TestInflight_GarbageLinesIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.jsonl")
	if err := os.WriteFile(path, []byte(
		`{"op":"start","job_id":"j1","agent_id":"w","ts":"2026-01-01T00:00:00Z"}`+"\n"+
			`not json`+"\n"+
			`{"op":"end","job_id":"j1","ts":"2026-01-01T00:00:01Z"}`+"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	out, err := l.Outstanding()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("expected garbage line skipped; outstanding = %+v", out)
	}
}

func TestInflight_WriteAfterCloseErrors(t *testing.T) {
	l, _ := openTest(t)
	_ = l.Close()
	if err := l.RecordStart("j", "w", "p"); err != ErrClosed {
		t.Errorf("want ErrClosed, got %v", err)
	}
}
