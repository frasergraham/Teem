// Package peeraware renders a "what your peers are doing" digest that
// the daemon writes into every leader's archmem memory once an hour.
//
// The daemon collects a Snapshot per registered team from in-memory
// state (plan, leader-status, registry, audit) and hands them all to
// Digest. For each team, Digest returns a markdown block describing
// every OTHER team's activity in the past window. The leader then sees
// this snippet on next chat / next pulse tick / next channel wake.
//
// Templating is deterministic — no LLM. The XP1 v1 goal is just cross-
// project awareness, not narrative summarisation.
package peeraware

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Snapshot is the input the daemon collects for one team. All slices
// may be empty; the renderer skips a peer entirely when nothing in this
// struct shows past-window activity.
type Snapshot struct {
	Team          string
	LeaderStatus  string    // current "what am I doing right now" line
	LeaderUpdated time.Time // when LeaderStatus was last written
	OpenTasks     []TaskBrief
	JustVerified  []TaskBrief
	JustBlocked   []TaskBrief
	ActiveAgents  []AgentBrief
	Decisions     []NoteBrief
	Blockers      []NoteBrief
}

// TaskBrief is the trimmed view of a plan.Task the digester needs.
type TaskBrief struct {
	ID    string
	Title string
	Stage string
}

// AgentBrief is the trimmed view of a registry entry.
type AgentBrief struct {
	ID    string
	Role  string
	State string
}

// NoteBrief is one decision / blocker recorded against a task in the
// past window.
type NoteBrief struct {
	TaskID string
	Text   string
	When   time.Time
}

// Digest renders the peers-of-team markdown for self. peers may include
// self — it's filtered out before rendering. Peers with zero activity
// in any of the four "moved in past hour" buckets AND no open tasks AND
// no active agents are skipped entirely. Returns "" when no peer has
// anything to report, so the daemon can skip the AppendEntry call.
//
// Output (per peer, one block):
//
//	## Peer: <team-name>  (snapshot 14:23 UTC)
//
//	- 2 tasks in flight: t-abcd (coding), t-efgh (reviewing)
//	- 1 task verified in last hour: t-1234 — "channels stdio shim"
//	- Workers active: worker-ada, reviewer-blake
//	- Leader: "Iterating on review feedback for channels work" (3m ago)
//	- Decisions logged: 1
//	- Blockers logged: 0
//
// Peers are emitted in alphabetical order so memory/dashboard diffs are
// stable.
func Digest(self string, peers []Snapshot, now time.Time) string {
	filtered := make([]Snapshot, 0, len(peers))
	for _, p := range peers {
		if p.Team == self {
			continue
		}
		if !hasActivity(p) {
			continue
		}
		filtered = append(filtered, p)
	}
	if len(filtered) == 0 {
		return ""
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		return filtered[i].Team < filtered[j].Team
	})
	var b strings.Builder
	b.WriteString("# Peer projects\n")
	for i, p := range filtered {
		if i > 0 {
			b.WriteString("\n")
		}
		writePeer(&b, p, now)
	}
	return b.String()
}

// hasActivity reports whether a peer has anything worth surfacing in
// the past-window snapshot. A non-empty LeaderStatus counts on its own
// because it's the most operator-relevant signal — even a peer with no
// task movement is informative if its leader has stated intent.
func hasActivity(p Snapshot) bool {
	return strings.TrimSpace(p.LeaderStatus) != "" ||
		len(p.OpenTasks) > 0 ||
		len(p.JustVerified) > 0 ||
		len(p.JustBlocked) > 0 ||
		len(p.ActiveAgents) > 0 ||
		len(p.Decisions) > 0 ||
		len(p.Blockers) > 0
}

func writePeer(b *strings.Builder, p Snapshot, now time.Time) {
	fmt.Fprintf(b, "## Peer: %s  (snapshot %s)\n", p.Team, now.UTC().Format("15:04 UTC"))
	if len(p.OpenTasks) > 0 {
		open := sortedByID(p.OpenTasks)
		fmt.Fprintf(b, "- %d %s in flight: %s\n",
			len(open), plural("task", len(open)), formatTaskList(open))
	}
	if len(p.JustVerified) > 0 {
		verified := sortedByID(p.JustVerified)
		fmt.Fprintf(b, "- %d %s verified in last hour: %s\n",
			len(verified), plural("task", len(verified)), formatTaskTitles(verified))
	}
	if len(p.JustBlocked) > 0 {
		blocked := sortedByID(p.JustBlocked)
		fmt.Fprintf(b, "- %d %s blocked in last hour: %s\n",
			len(blocked), plural("task", len(blocked)), formatTaskTitles(blocked))
	}
	if len(p.ActiveAgents) > 0 {
		fmt.Fprintf(b, "- Workers active: %s\n", formatAgents(p.ActiveAgents))
	}
	if strings.TrimSpace(p.LeaderStatus) != "" {
		ago := formatAgo(p.LeaderUpdated, now)
		if ago != "" {
			fmt.Fprintf(b, "- Leader: %q (%s)\n", p.LeaderStatus, ago)
		} else {
			fmt.Fprintf(b, "- Leader: %q\n", p.LeaderStatus)
		}
	}
	fmt.Fprintf(b, "- Decisions logged: %d\n", len(p.Decisions))
	fmt.Fprintf(b, "- Blockers logged: %d\n", len(p.Blockers))
}

func plural(noun string, n int) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

// maxTaskListEntries caps the number of comma-joined task entries in
// the in-flight digest line. Anything over is summarised as "…+N more"
// so a team with twenty open tasks doesn't bloat every peer's digest.
const maxTaskListEntries = 3

// maxTitleLen trims long task titles in the digest. Titles longer than
// this are cut and suffixed with "…".
const maxTitleLen = 60

func formatTaskList(ts []TaskBrief) string {
	n := len(ts)
	limit := n
	if limit > maxTaskListEntries {
		limit = maxTaskListEntries
	}
	parts := make([]string, 0, limit)
	for _, t := range ts[:limit] {
		stage := t.Stage
		if stage == "" {
			stage = "open"
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", t.ID, stage))
	}
	out := strings.Join(parts, ", ")
	if n > limit {
		out = out + fmt.Sprintf(", …+%d more", n-limit)
	}
	return out
}

func formatTaskTitles(ts []TaskBrief) string {
	n := len(ts)
	limit := n
	if limit > maxTaskListEntries {
		limit = maxTaskListEntries
	}
	parts := make([]string, 0, limit)
	for _, t := range ts[:limit] {
		title := strings.TrimSpace(t.Title)
		title = strings.ReplaceAll(title, "\n", " ")
		title = trimTitle(title, maxTitleLen)
		if title == "" {
			parts = append(parts, t.ID)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s — %q", t.ID, title))
	}
	out := strings.Join(parts, ", ")
	if n > limit {
		out = out + fmt.Sprintf(", …+%d more", n-limit)
	}
	return out
}

// trimTitle truncates s to at most n runes, appending "…" when cut.
// Rune-aware so we don't slice through a multi-byte character.
func trimTitle(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// sortedByID returns a copy of ts sorted by Task.ID. Used so that
// daemon-restart-to-restart digest output is byte-stable — the plan
// list iteration order isn't guaranteed across restarts.
func sortedByID(ts []TaskBrief) []TaskBrief {
	out := make([]TaskBrief, len(ts))
	copy(out, ts)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func formatAgents(as []AgentBrief) string {
	ids := make([]string, 0, len(as))
	for _, a := range as {
		ids = append(ids, a.ID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}

// formatAgo returns a short "Nm ago" / "Nh ago" relative timestamp.
// Empty string when t is zero so we don't render a misleading "0s ago".
func formatAgo(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
