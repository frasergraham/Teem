package main

import (
	_ "embed"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/leaderstatus"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/plan"
)

//go:embed ui_dashboard.html
var dashboardHTML string

//go:embed ui_agent_jobs.html
var agentJobsHTML string

//go:embed ui_job_detail.html
var jobDetailHTML string

//go:embed ui_task_flow.html
var taskFlowHTML string

// uiTemplates is the parsed bundle of the three SSR pages, built once
// at startup. They share a Funcs map so helpers (agoShort etc.) are
// available from each. Lookup by template name (see ExecuteTemplate).
var uiTemplates = newUITemplates()

// newUITemplates parses all three SSR templates and returns the bundle.
// Kept as a constructor so tests can rebuild a fresh copy when the
// embedded HTML changes (and so the wiring is reviewable in one place).
func newUITemplates() *template.Template {
	funcs := template.FuncMap{
		"agoShort":    agoShort,
		"timeShort":   timeShort,
		"expandable":  expandable,
		"capitalize":  capitalize,
		"toTitleCase": capitalize,
		"durShort":    durShort,
		"bytesShort":  bytesShort,
	}
	// dashboardHTML contains two named defines ("summary" and
	// "team_detail") plus a shared "ui_styles" sub-template. The
	// outer template name doesn't matter because we always render
	// via ExecuteTemplate by define name.
	t := template.Must(template.New("dashboard").Funcs(funcs).Parse(dashboardHTML))
	template.Must(t.New("agent_jobs").Parse(agentJobsHTML))
	template.Must(t.New("job_detail").Parse(jobDetailHTML))
	template.Must(t.New("task_flow").Parse(taskFlowHTML))
	return t
}

// expandable renders short strings inline and longer ones inside a
// collapsed <details> element so the dashboard/jobs tables don't
// truncate mid-thought. Output is template.HTML because the helper
// HTML-escapes the body itself; callers can hand it to the template
// without piping through another safe wrapper.
//
// Threshold (180 chars) is roughly the length where a one-liner stops
// fitting in a single tabular row at default body font.
func expandable(s string) template.HTML {
	if s == "" {
		return ""
	}
	const inlineMax = 180
	escaped := html.EscapeString(s)
	if len(s) <= inlineMax {
		return template.HTML(escaped)
	}
	// Trim back to the last valid UTF-8 rune boundary so a 2/3-byte
	// rune at the cap doesn't leave an invalid sequence inside the
	// preview HTML.
	end := inlineMax
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	preview := html.EscapeString(s[:end]) + "…"
	return template.HTML(
		`<details class="expandable"><summary>` + preview +
			`</summary><div class="expanded">` + escaped + `</div></details>`,
	)
}

// dashboardSnapshot is the data the summary index template renders.
// Computed fresh on every GET so the page doesn't have to lie about
// state. Tiles is the per-team rollup; the per-team detail page uses
// teamPageSnapshot instead.
type dashboardSnapshot struct {
	Endpoint     string
	StartedAt    time.Time
	UptimeAgo    string
	Tiles        []summaryTile
	NowFormatted string
}

// summaryTile is the per-team rollup rendered on the index page. The
// counters are intentionally precomputed so the template stays free of
// arithmetic. URL is the deep link to the per-team detail page.
type summaryTile struct {
	Name string
	// Slug is kept for backwards-compat with the dashboard template; it
	// now carries the team_id (the canonical routing key).
	Slug             string
	URL              string
	RegisteredAgo    string
	OpenTaskCount    int
	ActiveAgentCount int
	InFlight         int64
	CompletedToday   int
	// AwaitingApprovalCount surfaces "operator action needed" tasks on
	// the dashboard root tile so the count is visible without opening
	// the team page.
	AwaitingApprovalCount int
	LeaderStatusText      string // truncated to ~140 chars; empty when never set
	LeaderStatusAgo       string
	PulseRunning          bool
	PulsePaused           bool
	UnreadNotes           int
	BranchCount           int
}

// teamPageSnapshot wraps a single dashboardTeam for the detail page,
// reusing the existing per-team rendering. Carrying Endpoint/UptimeAgo
// keeps the header consistent with the summary index.
type teamPageSnapshot struct {
	Endpoint     string
	UptimeAgo    string
	NowFormatted string
	Team         dashboardTeam
	// Flash is the optional one-shot message surfaced after a POST
	// (e.g. the "Ping leader" button). Values: "pinged" (tick fired,
	// outcome not yet known) / "tick_ok" / "tick_failed" / "busy" /
	// "paused" — anything else renders no banner.
	Flash string
	// FlashDetail carries the trailing context for outcome-bearing
	// flashes: a duration like "1.2s" for tick_ok, or the leader's
	// error message for tick_failed. Empty for stateless flashes.
	FlashDetail string
}

type dashboardTeam struct {
	// ID is the canonical team id used in URLs (e.g. the ping form
	// posts to /control/teams/<id>/ping). Name is the human-readable
	// display label.
	ID            string
	Name          string
	RegisteredAgo string
	Agents        []dashboardAgent
	OpenTaskCount int
	OpenTasks     []dashboardTask
	// AwaitingApproval lists tasks currently in stage=awaiting_approval.
	// Rendered as a dedicated, prominent section at the top of the team
	// page with APPROVE / REJECT / COMMENT controls per row. These tasks
	// are ALSO included in OpenTaskCount (they're open) but pulled out
	// of OpenTasks so the main table isn't duplicated.
	AwaitingApproval []awaitingApprovalTask
	// Flash is a short status banner shown above the awaiting-approval
	// section after a successful form POST redirect ("task_approved",
	// "task_rejected", "task_commented", or ping outcomes via
	// flashFromQuery's whitelist).
	Flash          string
	Shelved        []dashboardTask
	RecentDone     []dashboardTask
	LeaderStatus   *leaderRow // pinned "leader" entry, if any
	OtherStatuses  []leaderRow
	PulseRunning   bool
	PulsePaused    bool
	PulseInterval  string
	PulseLastTick  string // "(never)" or "<duration> ago"
	PulseTickCount int64
	RecentEvents   []dashboardEvent
	UnreadNotes    int
	InFlight       int64
	// HasRepo reflects whether the team's registration carried a repo
	// root. False ⇒ render "(no repo)" in place of the branches section.
	HasRepo  bool
	Branches []dashboardBranch
}

type dashboardAgent struct {
	ID        string
	Role      string
	State     string
	LastSeen  string
	JobsURL   string
	Placement string
}

type leaderRow struct {
	AgentID        string
	Text           string
	UpdatedAgo     string
	CurrentTaskIDs []taskLink
}

type taskLink struct {
	ID  string
	URL string
}

// awaitingApprovalTask is the per-row data the team-detail page renders
// inside the "Awaiting approval" section. Carries the bits the operator
// needs to decide: title, id, the leader's notes preview, evidence
// links, and the URLs the inline form posts to.
type awaitingApprovalTask struct {
	ID           string
	Title        string
	NotesPreview string
	Notes        string // full notes (used by <details> for expansion)
	NotesLong    bool   // true when Notes exceeds the preview cap
	Evidence     []string
	StageAgo     string
	URL          string // /teams/<id>/tasks/<id> (deep link)
	ApproveURL   string
	RejectURL    string
	CommentURL   string
}

type dashboardTask struct {
	ID         string
	Title      string
	Status     string
	Stage      string
	StageAgo   string
	AssignedTo string
	// AssigneeActive is false when AssignedTo names a worker the
	// registry no longer treats as active (stopped, unregistered, or
	// never seen). The template uses this to mute the cell so it's
	// obvious nobody is currently driving the task.
	AssigneeActive bool
	// Stale is true when an active pipeline stage (planning/coding/
	// reviewing/integrating) names an inactive assignee — i.e. the task thinks
	// someone is working it but no one is. The template surfaces this
	// as a small STALE pill so the leader knows to re-assign or move
	// the task forward.
	Stale bool
	URL   string
}

type dashboardEvent struct {
	Time    string
	AgentID string
	Kind    string
	Message string
	JobID   string
	JobURL  string
}

// renderDashboard composes the summary index — a tile per registered
// team, each linking to /teams/<slug> for the deep view. Designed to
// read at-a-glance across the room: counters in big bold numerals.
func (d *daemon) renderDashboard(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	teams := make([]*registeredTeam, 0, len(d.teams))
	for _, rt := range d.teams {
		teams = append(teams, rt)
	}
	d.mu.Unlock()
	sort.Slice(teams, func(i, j int) bool { return teams[i].team.Name < teams[j].team.Name })

	state := readDaemonStateFileSafe()

	snap := dashboardSnapshot{
		Endpoint:     d.endpoint,
		StartedAt:    state.StartedAt,
		UptimeAgo:    agoShort(state.StartedAt),
		Tiles:        make([]summaryTile, 0, len(teams)),
		NowFormatted: time.Now().Local().Format("Mon Jan 2 15:04:05"),
	}
	for _, rt := range teams {
		snap.Tiles = append(snap.Tiles, teamTileSnapshot(rt))
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := uiTemplates.ExecuteTemplate(w, "summary", snap); err != nil {
		// Template errors land here once everything else has already
		// been written; surface to stderr but don't try to recover.
		fmt.Printf("[teemd] dashboard render: %v\n", err)
	}
}

// renderTeamPage serves the deep view for a single team at
// /teams/<slug>. The slug is the canonical filesystem / routing key
// (display name lives inside the page body). Accepts either the
// canonical id or the team's display name as the URL key — the
// resolver in daemon.go aliases them. Returns 404 when no team
// matches.
func (d *daemon) renderTeamPage(w http.ResponseWriter, r *http.Request, teamID string) {
	found := d.resolveTeam(teamID)
	if found == nil {
		http.NotFound(w, r)
		return
	}

	state := readDaemonStateFileSafe()
	team := teamSnapshot(found)
	// flash is set by the form-POST redirect (?flash=task_approved etc).
	// Whitelisted to a known set so a malicious link can't inject
	// arbitrary text into the page header.
	switch r.URL.Query().Get("flash") {
	case "task_approved":
		team.Flash = "Approved — task moved to coding"
	case "task_rejected":
		team.Flash = "Rejected — task shelved"
	case "task_commented":
		team.Flash = "Comment added"
	}
	flashTag := flashFromQuery(r.URL.Query().Get("flash"))
	flashDetail := ""
	// For flash=pinged we wait for the leader's pulse_tick audit event
	// since ?ping_ts=<unix> and upgrade the flash to tick_ok / tick_failed
	// once the tick lands. The team page meta-refreshes every 10s, so the
	// banner converges naturally — no JS poll required.
	if flashTag == "pinged" {
		if ts, ok := parseUnixSeconds(r.URL.Query().Get("ping_ts")); ok && found.auditSink != nil {
			if outcomeTag, detail, found := resolvePingOutcome(found.auditSink, ts); found {
				flashTag = outcomeTag
				flashDetail = detail
			}
		}
	}

	snap := teamPageSnapshot{
		Endpoint:     d.endpoint,
		UptimeAgo:    agoShort(state.StartedAt),
		NowFormatted: time.Now().Local().Format("Mon Jan 2 15:04:05"),
		Team:         team,
		Flash:        flashTag,
		FlashDetail:  flashDetail,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := uiTemplates.ExecuteTemplate(w, "team_detail", snap); err != nil {
		fmt.Printf("[teemd] team page render: %v\n", err)
	}
}

// teamTileSnapshot rolls a registered team up into its summary index
// tile. Reuses teamSnapshot for the counters that are already cheap to
// compute and layers on completed-today (which the deep view doesn't
// surface separately) and the leader-status one-liner.
func teamTileSnapshot(rt *registeredTeam) summaryTile {
	ts := teamSnapshot(rt)
	tile := summaryTile{
		Name:                  rt.team.Name,
		Slug:                  rt.team.ID,
		RegisteredAgo:         ts.RegisteredAgo,
		OpenTaskCount:         ts.OpenTaskCount,
		ActiveAgentCount:      len(ts.Agents),
		InFlight:              ts.InFlight,
		PulseRunning:          ts.PulseRunning,
		PulsePaused:           ts.PulsePaused,
		UnreadNotes:           ts.UnreadNotes,
		BranchCount:           len(ts.Branches),
		AwaitingApprovalCount: len(ts.AwaitingApproval),
	}
	tile.URL = "/teams/" + tile.Slug

	// Completed today: tasks whose Status is terminal (done or
	// abandoned) and whose UpdatedAt is at-or-after local midnight.
	// We re-scan the plan because teamSnapshot caps RecentDone at 5
	// — for the counter we want the full count.
	if rt.plan != nil {
		midnight := startOfLocalDay(time.Now())
		for _, t := range rt.plan.List(plan.Filter{}) {
			if t.Status != plan.StatusDone && t.Status != plan.StatusAbandoned {
				continue
			}
			if !t.UpdatedAt.Before(midnight) {
				tile.CompletedToday++
			}
		}
	}

	// Leader status one-liner: most recent entry where AgentID is
	// "leader". teamSnapshot has already pulled the pinned LeaderStatus
	// row out for us; if missing, look for any leader-keyed entry
	// directly (defensive — should be equivalent).
	if ts.LeaderStatus != nil {
		tile.LeaderStatusText = truncateForTile(ts.LeaderStatus.Text, 140)
		tile.LeaderStatusAgo = ts.LeaderStatus.UpdatedAgo
	}
	return tile
}

// startOfLocalDay returns the most recent midnight in the local zone.
// Used for the "completed today" counter so a task finished at 23:59
// drops out at midnight.
// flashFromQuery whitelists the ?flash= values the ping handler can
// set, so an attacker can't inject arbitrary text into the dashboard
// via the URL. Unknown values fall back to the empty string (no
// flash rendered).
func flashFromQuery(v string) string {
	switch v {
	case "pinged", "busy", "paused",
		"task_approved", "task_rejected", "task_commented":
		return v
	default:
		return ""
	}
}

// parseUnixSeconds parses the ?ping_ts=<unix> query parameter, rejecting
// negative / non-integer / unreasonably old values. The cap of one hour
// keeps stale shared/refreshed URLs from re-rendering a long-resolved
// tick as if it just happened.
func parseUnixSeconds(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	t := time.Unix(n, 0)
	if time.Since(t) > time.Hour || t.After(time.Now().Add(time.Minute)) {
		return time.Time{}, false
	}
	return t, true
}

// resolvePingOutcome scans the audit log for a leader pulse_tick event
// emitted on or after sinceTS (the moment we dispatched the ping). On
// hit it returns ("tick_ok"|"tick_failed", detailString, true). On miss
// (tick still in flight) it returns ("", "", false) and the caller
// keeps the "pinged — taking a turn now" banner; the page's 10s meta
// refresh will retry until the event lands.
//
// We pad the since cursor by one second because the audit timestamp is
// stamped at the start of Tick() while ping_ts is stamped just before
// safeGo dispatch — those can land in opposite microsecond orders.
func resolvePingOutcome(sink *audit.FileSink, sinceTS time.Time) (string, string, bool) {
	since := sinceTS.Add(-1 * time.Second)
	events, err := sink.Query("leader", since, 32)
	if err != nil {
		return "", "", false
	}
	for _, ev := range events {
		if ev.Kind != audit.Kind("pulse_tick") {
			continue
		}
		if errFlag, _ := ev.Meta["error"].(bool); errFlag {
			msg := ev.Message
			if msg == "" {
				msg = "(no error message)"
			}
			return "tick_failed", msg, true
		}
		// Success — surface duration when available so the operator can
		// see at-a-glance how long the leader's turn took.
		detail := ""
		if ms, ok := metaInt(ev.Meta["duration_ms"]); ok {
			detail = formatDurationMs(ms)
		}
		return "tick_ok", detail, true
	}
	return "", "", false
}

// metaInt narrows the audit event Meta map (which carries any) into an
// int64 millisecond duration. JSON round-trips numbers as float64, so we
// accept both shapes.
func metaInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}

// formatDurationMs renders a tick duration in a compact, operator-readable
// form: sub-second as "320ms", otherwise "1.2s".
func formatDurationMs(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

func startOfLocalDay(now time.Time) time.Time {
	loc := now.Location()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
}

// truncateForTile clamps a leader status blurb to a single-line preview
// suitable for a tile footer. UTF-8 safe: we back up to a valid rune
// boundary so we never split a multi-byte rune.
func truncateForTile(s string, max int) string {
	if len(s) <= max {
		return s
	}
	end := max
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}

// readDaemonStateFileSafe wraps readDaemonStateFile to swallow errors
// (we'd rather render an incomplete dashboard than fail open).
func readDaemonStateFileSafe() daemonStateFile {
	s, _, _ := readDaemonStateFile()
	return s
}

// teamSnapshot derives a per-team dashboard view. Reads from the
// registry, plan, audit (last ~20 events), pulse, and notes inbox.
// All read-only and cheap enough to do every page load.
func teamSnapshot(rt *registeredTeam) dashboardTeam {
	out := dashboardTeam{ID: rt.team.ID, Name: rt.team.Name}
	out.RegisteredAgo = agoShort(rt.registered)

	// Agents from the registry — hide fully-stopped agents only.
	// Provisioning and error states stay visible: an operator watching
	// a Fargate spin-up or a crashed worker needs that signal. Stopped
	// workers remain reachable at /teams/<team>/agents/<id>/jobs.
	entries := rt.registry.List()
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	// liveAgents = the set rendered under "Active agents" below. Used
	// to decide whether a task's AssignedTo is currently being worked
	// on or pointing at a worker that's gone.
	liveAgents := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.State != mcpsrv.StateStopped {
			liveAgents[e.ID] = true
		}
	}
	for _, e := range entries {
		if e.State == mcpsrv.StateStopped {
			continue
		}
		placement := "—"
		if a := rt.team.FindArchetypeByRole(e.Role); a != nil {
			placement = a.Placement
		}
		da := dashboardAgent{
			ID:        e.ID,
			Role:      e.Role,
			State:     string(e.State),
			JobsURL:   fmt.Sprintf("/teams/%s/agents/%s/jobs", rt.team.ID, e.ID),
			Placement: placement,
		}
		if !e.LastSeen.IsZero() {
			da.LastSeen = agoShort(e.LastSeen)
		} else {
			da.LastSeen = "—"
		}
		out.Agents = append(out.Agents, da)
		if e.State == mcpsrv.StateBusy {
			out.InFlight++
		}
	}

	// Plan: open tasks (sorted by stage so the pipeline reads
	// proposed → verified) and the 5 most-recently-completed tasks.
	if rt.plan != nil {
		all := rt.plan.List(plan.Filter{})
		var shelved []plan.Task
		var awaiting []plan.Task
		for _, t := range all {
			switch {
			case t.Stage == plan.StageAwaitingApproval:
				// Counts as "open" (still needs attention) but lives in
				// its own section so the operator-action-required tasks
				// jump out at the top of the page.
				out.OpenTaskCount++
				awaiting = append(awaiting, t)
			case t.Status.IsOpen():
				out.OpenTaskCount++
				out.OpenTasks = append(out.OpenTasks, taskToDashboardTask(rt.team.ID, t, liveAgents))
			case t.Status.IsShelved():
				shelved = append(shelved, t)
			}
		}
		// Awaiting-approval: newest-entered first by StageEnteredAt so a
		// fresh request for signoff sits at the top of the section.
		sort.SliceStable(awaiting, func(i, j int) bool {
			return awaiting[i].StageEnteredAt.After(awaiting[j].StageEnteredAt)
		})
		for _, t := range awaiting {
			out.AwaitingApproval = append(out.AwaitingApproval, taskToAwaitingApprovalTask(rt.team.ID, t))
		}
		// Sort open tasks by stage order then created.
		sort.SliceStable(out.OpenTasks, func(i, j int) bool {
			return stageOrder(out.OpenTasks[i].Stage) < stageOrder(out.OpenTasks[j].Stage)
		})
		// Shelved tasks: newest-shelved first so a task you just put
		// down is easy to find again. Not capped — the section exists
		// so the operator doesn't forget what they paused on.
		sort.Slice(shelved, func(i, j int) bool { return shelved[i].UpdatedAt.After(shelved[j].UpdatedAt) })
		for _, t := range shelved {
			out.Shelved = append(out.Shelved, taskToDashboardTask(rt.team.ID, t, liveAgents))
		}
		// Recent completed: tasks whose status moved to done, newest
		// first by UpdatedAt; capped to 5.
		var done []plan.Task
		for _, t := range all {
			if t.Status == plan.StatusDone || t.Status == plan.StatusAbandoned {
				done = append(done, t)
			}
		}
		sort.Slice(done, func(i, j int) bool { return done[i].UpdatedAt.After(done[j].UpdatedAt) })
		if len(done) > 5 {
			done = done[:5]
		}
		for _, t := range done {
			out.RecentDone = append(out.RecentDone, taskToDashboardTask(rt.team.ID, t, liveAgents))
		}
	}

	// Leader status board: leader pinned on top, others below.
	if rt.leaderStatus != nil {
		for _, e := range rt.leaderStatus.All() {
			row := leaderStatusToRow(rt.team.ID, e)
			if e.AgentID == "leader" {
				rcopy := row
				out.LeaderStatus = &rcopy
				continue
			}
			out.OtherStatuses = append(out.OtherStatuses, row)
		}
	}

	// Pulse status.
	if rt.pulse != nil {
		out.PulseRunning = rt.pulse.Running()
		out.PulsePaused = rt.pulse.Paused()
		out.PulseInterval = rt.pulse.Interval().String()
		out.PulseTickCount = rt.pulse.TickCount()
		if last := rt.pulse.LastTick(); !last.IsZero() {
			out.PulseLastTick = agoShort(last)
		} else {
			out.PulseLastTick = "(never)"
		}
	}

	// Recent audit events.
	if rt.auditSink != nil {
		events, _ := rt.auditSink.Query("", time.Now().Add(-30*time.Minute), 20)
		// Reverse to newest-first.
		for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
			events[i], events[j] = events[j], events[i]
		}
		for _, e := range events {
			if len(out.RecentEvents) >= 8 {
				break
			}
			de := dashboardEvent{
				Time:    timeShort(e.Timestamp),
				AgentID: e.AgentID,
				Kind:    string(e.Kind),
				Message: eventSummary(e),
				JobID:   e.JobID,
			}
			if e.JobID != "" {
				de.JobURL = fmt.Sprintf("/teams/%s/jobs/%s", rt.team.ID, e.JobID)
			}
			out.RecentEvents = append(out.RecentEvents, de)
		}
	}

	// Unread notes count — cheap call.
	if rt.notes != nil {
		notes, _ := rt.notes.Unread()
		out.UnreadNotes = len(notes)
	}

	// Active teem/* branches in the team's working tree. One git
	// invocation per render is fine at v1 scale; if branches counts
	// climb into the hundreds we can layer a small TTL cache here.
	out.HasRepo = rt.repoRoot != ""
	if out.HasRepo {
		out.Branches = listTeemBranches(rt.repoRoot, rt.registry, rt.team.ID)
	}
	return out
}

// taskToDashboardTask converts a plan.Task to the row shape rendered
// by the dashboard template. liveAgents is the set of currently active
// (non-stopped) agent ids; it's used to decide whether the task's
// AssignedTo is being actively worked or pointing at a worker that
// has gone away — the latter is rendered muted and flagged STALE when
// the stage is one where someone should be holding the task.
func taskToDashboardTask(team string, t plan.Task, liveAgents map[string]bool) dashboardTask {
	stageAgo := ""
	if !t.StageEnteredAt.IsZero() {
		stageAgo = agoShort(t.StageEnteredAt)
	}
	assigneeActive := t.AssignedTo == "" || liveAgents[t.AssignedTo]
	stale := false
	if t.AssignedTo != "" && !assigneeActive {
		switch t.Stage {
		case plan.StagePlanning, plan.StageCoding, plan.StageReviewing, plan.StageIntegrating:
			stale = true
		}
	}
	return dashboardTask{
		ID:             t.ID,
		Title:          t.Title,
		Status:         string(t.Status),
		Stage:          string(t.Stage),
		StageAgo:       stageAgo,
		AssignedTo:     t.AssignedTo,
		AssigneeActive: assigneeActive,
		Stale:          stale,
		URL:            fmt.Sprintf("/teams/%s/tasks/%s", team, t.ID),
	}
}

// taskToAwaitingApprovalTask packages a plan.Task in awaiting_approval
// stage for the dashboard's prominent "Awaiting approval" section,
// pre-baking the form action URLs and a clamped notes preview.
func taskToAwaitingApprovalTask(team string, t plan.Task) awaitingApprovalTask {
	const previewMax = 200
	preview := t.Notes
	long := false
	if len(preview) > previewMax {
		end := previewMax
		for end > 0 && !utf8.RuneStart(preview[end]) {
			end--
		}
		preview = preview[:end] + "…"
		long = true
	}
	stageAgo := ""
	if !t.StageEnteredAt.IsZero() {
		stageAgo = agoShort(t.StageEnteredAt)
	}
	base := fmt.Sprintf("/teams/%s/tasks/%s", team, t.ID)
	return awaitingApprovalTask{
		ID:           t.ID,
		Title:        t.Title,
		NotesPreview: preview,
		Notes:        t.Notes,
		NotesLong:    long,
		Evidence:     append([]string(nil), t.Evidence...),
		StageAgo:     stageAgo,
		URL:          base,
		ApproveURL:   base + "/approve",
		RejectURL:    base + "/reject",
		CommentURL:   base + "/comment",
	}
}

// leaderStatusToRow converts a leaderstatus.Entry to the dashboard
// row shape, resolving task ids to clickable links.
func leaderStatusToRow(team string, e leaderstatus.Entry) leaderRow {
	r := leaderRow{
		AgentID:    e.AgentID,
		Text:       e.Text,
		UpdatedAgo: agoShort(e.UpdatedAt),
	}
	for _, id := range e.CurrentTaskIDs {
		r.CurrentTaskIDs = append(r.CurrentTaskIDs, taskLink{
			ID:  id,
			URL: fmt.Sprintf("/teams/%s/tasks/%s", team, id),
		})
	}
	return r
}

// stageOrder maps a stage to its display index so the dashboard can
// sort open tasks left-to-right along the pipeline. Unknown stages
// sort last.
func stageOrder(s string) int {
	switch plan.Stage(s) {
	case plan.StageProposed:
		return 0
	case plan.StageSpecced:
		return 1
	case plan.StageAwaitingApproval:
		return 2
	case plan.StagePlanning:
		return 3
	case plan.StageCoding:
		return 4
	case plan.StageReviewing:
		return 5
	case plan.StageIntegrating:
		return 6
	case plan.StageVerified:
		return 7
	case plan.StageBlocked:
		return 8
	case plan.StageAbandoned:
		return 9
	}
	return 99
}

// eventSummary picks the most useful one-liner for an audit event.
// Kinds with their own content (job_received, pulse_tick) get their
// prompt/text; lifecycle-only kinds get the message field.
func eventSummary(e audit.Event) string {
	switch e.Kind {
	case audit.KindJobReceived:
		if p, ok := e.Meta["prompt"].(string); ok {
			return p
		}
	case audit.KindJobComplete:
		if o, ok := e.Meta["output"].(string); ok && o != "" {
			return o
		}
		if n, ok := e.Meta["output_bytes"].(float64); ok {
			return fmt.Sprintf("%d bytes returned", int(n))
		}
	}
	return e.Message
}

// agoShort returns a natural-language relative time: "just now",
// "5s ago", "2m ago", "3d ago". Self-contained so templates don't
// have to append " ago" themselves. Empty for zero times.
func agoShort(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Second:
		return "just now"
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

// timeShort renders a clock-only timestamp suitable for an audit
// list. Local time so the operator's eyes don't have to convert.
func timeShort(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("15:04:05")
}

// durShort renders a non-zero duration compactly. Zero returns the
// empty string so templates can hide the field for in-flight jobs.
func durShort(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", int(d/time.Millisecond))
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d/time.Minute), int((d%time.Minute)/time.Second))
	}
	return d.Truncate(time.Second).String()
}

// bytesShort renders a byte count with a binary-prefix suffix. Empty
// when n == 0 so templates can hide the field for missing transcripts.
func bytesShort(n int) string {
	if n <= 0 {
		return ""
	}
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%dB", n)
	case n < k*k:
		return fmt.Sprintf("%.1fKB", float64(n)/k)
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/(k*k))
	}
}

// capitalize lowercases everything except the first character.
// Used for state badges (busy → "Busy", running → "Running").
func capitalize(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}
