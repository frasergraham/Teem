package main

import (
	"testing"

	"github.com/frasergraham/teem/internal/audit"
)

func TestPathToSlug(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"markdown under docs", "docs/usage-monitor.md", "docs-usage-monitor-md"},
		{"uppercase folded", "Docs/Plan.MD", "docs-plan-md"},
		{"underscores and spaces collapse", "docs/my plan_v2.md", "docs-my-plan-v2-md"},
		{"runs of separators dedup", "docs///foo...md", "docs-foo-md"},
		{"leading/trailing separators trimmed", "/docs/foo.md/", "docs-foo-md"},
		{"empty input", "", ""},
		{"all-separator input", "///...", ""},
		{"non-ascii runes silently dropped", "docs/résumé.md", "docs-rsum-md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pathToSlug(tc.in)
			if got != tc.want {
				t.Errorf("pathToSlug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsPlanShaped(t *testing.T) {
	cases := []struct {
		name  string
		files []planFile
		want  bool
	}{
		{"empty set is not plan-shaped", nil, false},
		{"all docs/*.md is plan-shaped", []planFile{
			{Path: "docs/a.md", IsMarkdown: true},
			{Path: "docs/sub/b.md", IsMarkdown: true},
		}, true},
		{"one non-md outside docs disqualifies", []planFile{
			{Path: "docs/a.md", IsMarkdown: true},
			{Path: "main.go", IsMarkdown: false},
		}, false},
		{"md file outside docs disqualifies", []planFile{
			{Path: "README.md", IsMarkdown: true},
		}, false},
		{"non-md file under docs disqualifies", []planFile{
			{Path: "docs/image.png", IsMarkdown: false},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPlanShaped(tc.files); got != tc.want {
				t.Errorf("isPlanShaped(%v) = %v, want %v", tc.files, got, tc.want)
			}
		})
	}
}

func TestResolveEvidenceRows(t *testing.T) {
	t.Run("empty job list returns nil", func(t *testing.T) {
		got := resolveEvidenceRows(nil, nil, "", "team-x")
		if got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("job_id mapped to agent_id via first-event-wins", func(t *testing.T) {
		events := []audit.Event{
			{JobID: "j1", AgentID: "worker-ada"},
			{JobID: "j1", AgentID: "ignored"}, // later events don't override
			{JobID: "j2", AgentID: "worker-ben"},
			{JobID: "", AgentID: "skip-no-jobid"}, // skipped
			{JobID: "j3", AgentID: ""},            // skipped, no AgentID
		}
		rows := resolveEvidenceRows(events, []string{"j1", "j2", "j3", "missing"}, "", "team-x")
		if len(rows) != 4 {
			t.Fatalf("got %d rows, want 4", len(rows))
		}
		// j1 → worker-ada with branch ref
		if rows[0].JobID != "j1" || rows[0].AgentID != "worker-ada" || rows[0].BranchRef != "teem/worker-ada" {
			t.Errorf("j1 row = %+v", rows[0])
		}
		// j2 → worker-ben
		if rows[1].JobID != "j2" || rows[1].AgentID != "worker-ben" || rows[1].BranchRef != "teem/worker-ben" {
			t.Errorf("j2 row = %+v", rows[1])
		}
		// j3 had no AgentID → empty
		if rows[2].JobID != "j3" || rows[2].AgentID != "" || rows[2].BranchRef != "" {
			t.Errorf("j3 row = %+v", rows[2])
		}
		// missing → empty
		if rows[3].JobID != "missing" || rows[3].AgentID != "" {
			t.Errorf("missing row = %+v", rows[3])
		}
	})

	t.Run("unsafe agent_id leaves BranchRef blank", func(t *testing.T) {
		events := []audit.Event{
			{JobID: "j1", AgentID: "bad/agent"},
		}
		rows := resolveEvidenceRows(events, []string{"j1"}, "", "team-x")
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1", len(rows))
		}
		// isSafeID rejects '/' so AgentID stays blank and no branch ref is built.
		if rows[0].AgentID != "" || rows[0].BranchRef != "" {
			t.Errorf("unsafe id should be dropped: %+v", rows[0])
		}
	})

	t.Run("blank repoRoot skips git lookup; PlanFiles stays nil", func(t *testing.T) {
		events := []audit.Event{{JobID: "j1", AgentID: "worker-ada"}}
		rows := resolveEvidenceRows(events, []string{"j1"}, "", "team-x")
		if len(rows) != 1 || rows[0].PlanFiles != nil || rows[0].PlanShaped {
			t.Errorf("got %+v, want zero PlanFiles + PlanShaped=false", rows[0])
		}
	})
}
