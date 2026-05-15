package archmem

import (
	"strings"
	"testing"
	"time"
)

// TestBuildDigestPrompt_IncludesEntriesAndPriorDigest pins the prompt
// contract: every entry the caller passes must appear in the prompt
// (so the LLM has full context), and the prior digest is included with
// a "may be stale" caveat so the model knows it can replace text.
func TestBuildDigestPrompt_IncludesEntriesAndPriorDigest(t *testing.T) {
	entries := []Entry{
		{
			Timestamp: time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
			AgentID:   "worker-ada",
			JobID:     "j1",
			Status:    "done",
			Summary:   "touched executor.go",
		},
		{
			Timestamp: time.Date(2026, 5, 13, 13, 0, 0, 0, time.UTC),
			AgentID:   "worker-blake",
			JobID:     "j2",
			Status:    "done",
			Summary:   "added pulse_test cases",
		},
	}
	prev := "Previous focus: executor refactor."
	got := buildDigestPrompt(entries, prev)

	for _, want := range []string{
		"You distill activity logs",         // system framing
		"may be stale",                      // caveat on prev digest
		"Previous focus: executor refactor", // prev digest body
		"Recent entries (oldest first)",     // section header
		"worker-ada",
		"touched executor.go",
		"worker-blake",
		"added pulse_test cases",
		"4-6 line markdown digest", // task instruction
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q\n--- prompt ---\n%s", want, got)
		}
	}
}

// TestBuildDigestPrompt_OmitsPriorDigestWhenEmpty verifies first-run
// behaviour: no "Previous digest" section when prevDigest == "".
func TestBuildDigestPrompt_OmitsPriorDigestWhenEmpty(t *testing.T) {
	entries := []Entry{
		{
			Timestamp: time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
			AgentID:   "worker-1",
			JobID:     "j",
			Status:    "done",
			Summary:   "first entry",
		},
	}
	got := buildDigestPrompt(entries, "")
	if strings.Contains(got, "Previous digest") {
		t.Errorf("expected no Previous digest section on first run, got:\n%s", got)
	}
	if !strings.Contains(got, "first entry") {
		t.Errorf("entry summary missing from prompt:\n%s", got)
	}
}

// TestBuildDigestPrompt_EntriesInOrder verifies entries appear in the
// order they were passed in (oldest-first is the caller's contract;
// the prompt mustn't shuffle them).
func TestBuildDigestPrompt_EntriesInOrder(t *testing.T) {
	entries := []Entry{
		{Timestamp: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), AgentID: "a-1", JobID: "j", Status: "done", Summary: "older"},
		{Timestamp: time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC), AgentID: "a-2", JobID: "j", Status: "done", Summary: "newer"},
	}
	got := buildDigestPrompt(entries, "")
	iOlder := strings.Index(got, "older")
	iNewer := strings.Index(got, "newer")
	if iOlder < 0 || iNewer < 0 {
		t.Fatalf("expected both entries in prompt:\n%s", got)
	}
	if iOlder > iNewer {
		t.Errorf("entries reordered — older should appear before newer:\n%s", got)
	}
}
