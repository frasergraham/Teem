package peeraware

import (
	"strings"
	"testing"
	"time"
)

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestDigest_Empty(t *testing.T) {
	now := mustTime("2026-05-14T14:23:00Z")

	// No peers at all.
	if got := Digest("alpha", nil, now); got != "" {
		t.Fatalf("expected empty digest, got:\n%s", got)
	}

	// Peers exist but none have any signal — LeaderStatus now counts
	// as activity (see TestDigest_HasActivity_LeaderStatusOnly), so the
	// inert peers must leave it blank too.
	peers := []Snapshot{
		{Team: "beta"},
		{Team: "gamma"},
	}
	if got := Digest("alpha", peers, now); got != "" {
		t.Fatalf("expected empty digest when no peer has activity, got:\n%s", got)
	}
}

func TestDigest_OnePeer(t *testing.T) {
	now := mustTime("2026-05-14T14:23:00Z")
	peers := []Snapshot{
		{
			Team:          "beta",
			LeaderStatus:  "Iterating on review feedback",
			LeaderUpdated: now.Add(-3 * time.Minute),
			OpenTasks: []TaskBrief{
				{ID: "t-abcd", Title: "wire channels", Stage: "coding"},
				{ID: "t-efgh", Title: "review fixups", Stage: "coding"},
			},
			JustVerified: []TaskBrief{
				{ID: "t-1234", Title: "channels stdio shim", Stage: "verified"},
			},
			ActiveAgents: []AgentBrief{
				{ID: "worker-ada", Role: "worker", State: "busy"},
				{ID: "reviewer-blake", Role: "reviewer", State: "running"},
			},
			Decisions: []NoteBrief{{TaskID: "t-abcd", Text: "use mcp-go", When: now.Add(-10 * time.Minute)}},
		},
	}
	got := Digest("alpha", peers, now)
	if got == "" {
		t.Fatalf("expected non-empty digest")
	}

	checks := []string{
		"## Peer: beta",
		"snapshot 14:23 UTC",
		"2 tasks in flight",
		"t-abcd (coding)",
		"t-efgh (coding)",
		"1 task verified in last hour",
		`t-1234 — "channels stdio shim"`,
		"Workers active: reviewer-blake, worker-ada",
		`Leader: "Iterating on review feedback" (3m ago)`,
		"Decisions logged: 1",
		"Blockers logged: 0",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("digest missing %q\nfull output:\n%s", want, got)
		}
	}
}

func TestDigest_SkipsSelf(t *testing.T) {
	now := mustTime("2026-05-14T14:23:00Z")
	peers := []Snapshot{
		{
			Team:      "alpha",
			OpenTasks: []TaskBrief{{ID: "t-self", Title: "should not appear", Stage: "coding"}},
		},
		{
			Team:      "beta",
			OpenTasks: []TaskBrief{{ID: "t-peer", Title: "should appear", Stage: "coding"}},
		},
	}
	got := Digest("alpha", peers, now)
	if strings.Contains(got, "alpha") {
		t.Errorf("self team must be excluded; output:\n%s", got)
	}
	if strings.Contains(got, "t-self") {
		t.Errorf("self team task leaked into output; output:\n%s", got)
	}
	if !strings.Contains(got, "## Peer: beta") {
		t.Errorf("expected peer beta block; output:\n%s", got)
	}
}

func TestDigest_StableOrdering(t *testing.T) {
	now := mustTime("2026-05-14T14:23:00Z")
	peers := []Snapshot{
		{Team: "gamma", OpenTasks: []TaskBrief{{ID: "t-g", Stage: "coding"}}},
		{Team: "alpha", OpenTasks: []TaskBrief{{ID: "t-a", Stage: "coding"}}},
		{Team: "delta", OpenTasks: []TaskBrief{{ID: "t-d", Stage: "coding"}}},
		{Team: "beta", OpenTasks: []TaskBrief{{ID: "t-b", Stage: "coding"}}},
	}
	got := Digest("self", peers, now)

	idxAlpha := strings.Index(got, "## Peer: alpha")
	idxBeta := strings.Index(got, "## Peer: beta")
	idxDelta := strings.Index(got, "## Peer: delta")
	idxGamma := strings.Index(got, "## Peer: gamma")
	if idxAlpha < 0 || idxBeta < 0 || idxDelta < 0 || idxGamma < 0 {
		t.Fatalf("missing one or more peer blocks; output:\n%s", got)
	}
	if !(idxAlpha < idxBeta && idxBeta < idxDelta && idxDelta < idxGamma) {
		t.Errorf("peers not in alphabetical order: alpha=%d beta=%d delta=%d gamma=%d\noutput:\n%s",
			idxAlpha, idxBeta, idxDelta, idxGamma, got)
	}

	// Re-running with a different input order must produce identical bytes.
	shuffled := []Snapshot{peers[2], peers[0], peers[3], peers[1]}
	if got2 := Digest("self", shuffled, now); got2 != got {
		t.Errorf("digest output is not stable across input orderings:\nfirst:\n%s\nsecond:\n%s", got, got2)
	}
}

func TestDigest_HasActivity_LeaderStatusOnly(t *testing.T) {
	now := mustTime("2026-05-14T14:23:00Z")
	peers := []Snapshot{{
		Team:          "beta",
		LeaderStatus:  "Investigating flaky test in CI",
		LeaderUpdated: now.Add(-2 * time.Minute),
	}}
	got := Digest("alpha", peers, now)
	if got == "" {
		t.Fatalf("expected non-empty digest for peer with only LeaderStatus")
	}
	if !strings.Contains(got, "## Peer: beta") {
		t.Errorf("missing peer header; got:\n%s", got)
	}
	if !strings.Contains(got, `Leader: "Investigating flaky test in CI" (2m ago)`) {
		t.Errorf("leader status not rendered; got:\n%s", got)
	}
}

func TestDigest_TitlesAreTrimmed(t *testing.T) {
	now := mustTime("2026-05-14T14:23:00Z")
	long := strings.Repeat("A", 200)
	peers := []Snapshot{{
		Team: "beta",
		JustVerified: []TaskBrief{
			{ID: "t-long", Title: long, Stage: "verified"},
		},
	}}
	got := Digest("alpha", peers, now)
	if strings.Contains(got, long) {
		t.Errorf("untruncated title leaked into digest:\n%s", got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("expected ellipsis suffix on truncated title; got:\n%s", got)
	}
}

func TestDigest_TaskListCappedAtThree(t *testing.T) {
	now := mustTime("2026-05-14T14:23:00Z")
	tasks := []TaskBrief{
		{ID: "t-a", Stage: "coding"},
		{ID: "t-b", Stage: "coding"},
		{ID: "t-c", Stage: "coding"},
		{ID: "t-d", Stage: "coding"},
		{ID: "t-e", Stage: "coding"},
	}
	peers := []Snapshot{{Team: "beta", OpenTasks: tasks}}
	got := Digest("alpha", peers, now)
	if !strings.Contains(got, "5 tasks in flight") {
		t.Errorf("expected total count of 5 in header; got:\n%s", got)
	}
	if !strings.Contains(got, "…+2 more") {
		t.Errorf("expected '…+2 more' suffix; got:\n%s", got)
	}
	if strings.Contains(got, "t-d") || strings.Contains(got, "t-e") {
		t.Errorf("tasks beyond cap leaked into digest; got:\n%s", got)
	}
}

func TestDigest_StableOrdering_WithinPeer(t *testing.T) {
	now := mustTime("2026-05-14T14:23:00Z")
	peers := []Snapshot{{
		Team: "beta",
		OpenTasks: []TaskBrief{
			{ID: "t-c", Stage: "coding"},
			{ID: "t-a", Stage: "coding"},
			{ID: "t-b", Stage: "coding"},
		},
		JustVerified: []TaskBrief{
			{ID: "t-9", Title: "nine", Stage: "verified"},
			{ID: "t-1", Title: "one", Stage: "verified"},
			{ID: "t-5", Title: "five", Stage: "verified"},
		},
	}}
	first := Digest("alpha", peers, now)
	// Re-shuffle the inner slices; output must be byte-identical.
	peers[0].OpenTasks = []TaskBrief{
		{ID: "t-b", Stage: "coding"},
		{ID: "t-c", Stage: "coding"},
		{ID: "t-a", Stage: "coding"},
	}
	peers[0].JustVerified = []TaskBrief{
		{ID: "t-5", Title: "five", Stage: "verified"},
		{ID: "t-9", Title: "nine", Stage: "verified"},
		{ID: "t-1", Title: "one", Stage: "verified"},
	}
	second := Digest("alpha", peers, now)
	if first != second {
		t.Errorf("within-peer ordering not stable:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	// And the actual order in the output must be by ID.
	iA := strings.Index(first, "t-a")
	iB := strings.Index(first, "t-b")
	iC := strings.Index(first, "t-c")
	if !(iA < iB && iB < iC) {
		t.Errorf("open tasks not sorted by ID: a=%d b=%d c=%d\noutput:\n%s", iA, iB, iC, first)
	}
}
