package main

import (
	_ "embed"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

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
	preview := html.EscapeString(s[:inlineMax]) + "…"
	return template.HTML(
		`<details class="expandable"><summary>` + preview +
			`</summary><div class="expanded">` + escaped + `</div></details>`,
	)
}

// dashboardSnapshot is the data the template renders. Computed fresh
// on every GET so the page doesn't have to lie about state.
type dashboardSnapshot struct {
	Endpoint     string
	StartedAt    time.Time
	UptimeAgo    string
	Teams        []dashboardTeam
	NowFormatted string
}

type dashboardTeam struct {
	Name           string
	RegisteredAgo  string
	Agents         []dashboardAgent
	OpenTaskCount  int
	OpenTasks      []dashboardTask
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

type dashboardTask struct {
	ID         string
	Title      string
	Status     string
	Stage      string
	StageAgo   string
	AssignedTo string
	URL        string
}

type dashboardEvent struct {
	Time    string
	AgentID string
	Kind    string
	Message string
	JobID   string
	JobURL  string
}

// renderDashboard composes the snapshot for the daemon's current state
// and writes the rendered HTML.
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
		Teams:        make([]dashboardTeam, 0, len(teams)),
		NowFormatted: time.Now().Local().Format("Mon Jan 2 15:04:05"),
	}
	for _, rt := range teams {
		snap.Teams = append(snap.Teams, teamSnapshot(rt))
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := uiTemplates.ExecuteTemplate(w, "dashboard", snap); err != nil {
		// Template errors land here once everything else has already
		// been written; surface to stderr but don't try to recover.
		fmt.Printf("[teemd] dashboard render: %v\n", err)
	}
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
	out := dashboardTeam{Name: rt.team.Name}
	out.RegisteredAgo = agoShort(rt.registered)

	// Agents from the registry — filtered to "active" (running or
	// busy). Stopped workers stay in the audit log + per-agent jobs
	// page; they shouldn't crowd the live dashboard.
	entries := rt.registry.List()
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	for _, e := range entries {
		if e.State != mcpsrv.StateRunning && e.State != mcpsrv.StateBusy {
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
			JobsURL:   fmt.Sprintf("/teams/%s/agents/%s/jobs", rt.team.Name, e.ID),
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
		for _, t := range all {
			if t.Status.IsOpen() {
				out.OpenTaskCount++
				out.OpenTasks = append(out.OpenTasks, taskToDashboardTask(rt.team.Name, t))
			}
		}
		// Sort open tasks by stage order then created.
		sort.SliceStable(out.OpenTasks, func(i, j int) bool {
			return stageOrder(out.OpenTasks[i].Stage) < stageOrder(out.OpenTasks[j].Stage)
		})
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
			out.RecentDone = append(out.RecentDone, taskToDashboardTask(rt.team.Name, t))
		}
	}

	// Leader status board: leader pinned on top, others below.
	if rt.leaderStatus != nil {
		for _, e := range rt.leaderStatus.All() {
			row := leaderStatusToRow(rt.team.Name, e)
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
				de.JobURL = fmt.Sprintf("/teams/%s/jobs/%s", rt.team.Name, e.JobID)
			}
			out.RecentEvents = append(out.RecentEvents, de)
		}
	}

	// Unread notes count — cheap call.
	if rt.notes != nil {
		notes, _ := rt.notes.Unread()
		out.UnreadNotes = len(notes)
	}
	return out
}

// taskToDashboardTask converts a plan.Task to the row shape rendered
// by the dashboard template.
func taskToDashboardTask(team string, t plan.Task) dashboardTask {
	stageAgo := ""
	if !t.StageEnteredAt.IsZero() {
		stageAgo = agoShort(t.StageEnteredAt)
	}
	return dashboardTask{
		ID:         t.ID,
		Title:      t.Title,
		Status:     string(t.Status),
		Stage:      string(t.Stage),
		StageAgo:   stageAgo,
		AssignedTo: t.AssignedTo,
		URL:        fmt.Sprintf("/teams/%s/tasks/%s", team, t.ID),
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
	case plan.StageBuilding:
		return 2
	case plan.StageInReview:
		return 3
	case plan.StageMerging:
		return 4
	case plan.StageVerified:
		return 5
	case plan.StageBlocked:
		return 6
	case plan.StageAbandoned:
		return 7
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
