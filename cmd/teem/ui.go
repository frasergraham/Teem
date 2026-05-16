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
	"github.com/frasergraham/teem/internal/pulse"
	"github.com/frasergraham/teem/internal/team"
	"github.com/frasergraham/teem/internal/usage"
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
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	RegisteredAgo string           `json:"registered_ago"`
	Agents        []dashboardAgent `json:"agents"`
	OpenTaskCount int              `json:"open_task_count"`
	OpenTasks     []dashboardTask  `json:"open_tasks"`
	// AwaitingApproval lists tasks currently in stage=awaiting_approval.
	// Rendered as a dedicated, prominent section at the top of the team
	// page with APPROVE / REJECT / COMMENT controls per row. These tasks
	// are ALSO included in OpenTaskCount (they're open) but pulled out
	// of OpenTasks so the main table isn't duplicated.
	AwaitingApproval []awaitingApprovalTask `json:"awaiting_approval"`
	// Flash is a short status banner shown above the awaiting-approval
	// section after a successful form POST redirect ("task_approved",
	// "task_rejected", "task_commented", or ping outcomes via
	// flashFromQuery's whitelist).
	Flash          string          `json:"flash,omitempty"`
	Shelved        []dashboardTask `json:"shelved"`
	RecentDone     []dashboardTask `json:"recent_done"`
	LeaderStatus   *leaderRow      `json:"leader_status"`
	OtherStatuses  []leaderRow     `json:"other_statuses"`
	PulseRunning   bool            `json:"pulse_running"`
	PulsePaused    bool            `json:"pulse_paused"`
	PulseInterval  string          `json:"pulse_interval"`
	PulseLastTick  string          `json:"pulse_last_tick"`
	PulseTickCount int64           `json:"pulse_tick_count"`
	// Pulse is the richer per-team pulse view used by the bridge-console
	// pulse-management panel: lamp toggle, interval input, wake-prompt
	// textarea. Mirrors the top-level Pulse* fields above for the
	// existing header pill / heading line so we don't have to rewrite
	// callers.
	Pulse        pulseSnapshot    `json:"pulse"`
	RecentEvents []dashboardEvent `json:"recent_events"`
	UnreadNotes  int              `json:"unread_notes"`
	InFlight     int64            `json:"in_flight"`
	// HasRepo reflects whether the team's registration carried a repo
	// root. False ⇒ render "(no repo)" in place of the branches section.
	HasRepo  bool             `json:"has_repo"`
	Branches teamPageBranches `json:"branches"`
	// Hero is the "page-header" summary: big bold counters, agent
	// chips per archetype, and today's task pipeline as a stacked bar.
	Hero teamHero `json:"hero"`
	// Workers is the "bridge-console" active-workers manifest rendered
	// directly under the hero/status panel. One row per non-stopped
	// agent, ordered as Agents is (alphabetical by id). Activity is
	// derived from the leader-status board first, falling back to the
	// agent's assigned open tasks.
	Workers []workerRow `json:"workers"`
	// StatusHeadline is the short editorial line rendered in the
	// status-panel hero: today's leader-status text, or a quiet-day
	// placeholder when the leader hasn't posted one.
	StatusHeadline string `json:"status_headline"`
	// Decisions is the unified "operator action needed" panel mixing
	// awaiting-approval tasks, agent questions (record_decision with
	// severity=question), and open blockers (record_blocker against a
	// task still at stage=blocked). Sorted newest-first by timestamp.
	Decisions []decisionRow `json:"decisions"`
	// Usage is the daily-token-budget card rendered near the top of the
	// team-detail page. Nil when the daemon has no Aggregator wired (the
	// card is suppressed in that case).
	Usage *usageSnapshot `json:"usage"`
	// HasPricing is true when ~/.teem/pricing.yaml loaded with at least
	// one priced model. Drives whether the dashboard's Cost column and
	// hero "Today's spend" line render at all — absent pricing means
	// the dashboard hides cost UI rather than rendering $0.
	HasPricing bool `json:"has_pricing"`
	// PricingStale is true when the pricing.yaml mtime is older than
	// usage.StaleAge. The hero shows a small "(stale)" hint next to
	// Today's spend so the operator knows their numbers may have drifted
	// from Anthropic's current list prices.
	PricingStale bool `json:"pricing_stale"`
	// HeroSpendUSD is the dollar total of every KindUsageEvent emitted
	// since local midnight. Computed by usage.TodaysSpend from the raw
	// audit stream so the daily total stays correct even when per-task
	// numbers double-count cross-linked jobs.
	HeroSpendUSD float64 `json:"hero_spend_usd"`
	// HeroSpendDisplay is the pre-formatted "$X.XX" string the template
	// renders. Kept as a string so the template doesn't need a custom
	// formatter func; empty when HasPricing is false.
	HeroSpendDisplay string `json:"hero_spend_display"`
}

// usageSnapshot is the data behind the team-detail "Usage" card. Built
// from the daemon-global usage.Aggregator. Configured=false → the
// operator hasn't set daily_token_budget; the card shows the
// configuration hint instead of the bar.
type usageSnapshot struct {
	Configured  bool      `json:"configured"`
	Used        int64     `json:"used"`
	Cap         int64     `json:"cap"`
	PercentUsed float64   `json:"percent_used"`
	Throttle    bool      `json:"throttle"`
	NextReset   time.Time `json:"next_reset"`
	LastReset   time.Time `json:"last_reset"`
	// NextResetIn is the formatted "in 4h 23m" countdown. NextResetAbs
	// is the wall-clock tooltip (local time). Both empty when the
	// anchor parse fails (defensive — operator sees no countdown).
	NextResetIn  string       `json:"next_reset_in"`
	NextResetAbs string       `json:"next_reset_abs"`
	LastResetAbs string       `json:"last_reset_abs"`
	PerModel     []modelUsage `json:"per_model"`
	BarColour    string       `json:"bar_colour"` // "green" | "amber" | "red"
}

// modelUsage is one row in the per-model breakdown. Total is
// Input+Output+CacheCreate (matches the billable definition used by
// the throttle); CacheRead is reported separately so the operator can
// see read-side caching activity without it inflating the cap.
type modelUsage struct {
	Model       string `json:"model"`
	Input       int64  `json:"input"`
	Output      int64  `json:"output"`
	CacheCreate int64  `json:"cache_create"`
	CacheRead   int64  `json:"cache_read"`
	Total       int64  `json:"total"`
}

// decisionRow is one row in the unified Decisions panel. TypeClass is
// the CSS modifier the template appends to .decision-stripe and
// .decision-row ("approval" / "question" / "blocker"); Stripe is the
// hex colour the inline style="background:..." uses. Approval is only
// set for Type==APPROVAL — it carries the rich evidence/plan-artifact
// payload so the approval card preserves its existing rendering.
type decisionRow struct {
	Type      string                `json:"type"`
	TypeClass string                `json:"type_class"`
	TaskID    string                `json:"task_id"`
	Title     string                `json:"title"`
	Summary   string                `json:"summary"`
	Age       string                `json:"age"`
	URL       string                `json:"url"`
	Stripe    string                `json:"stripe"`
	Timestamp time.Time             `json:"timestamp"`
	Actions   []decisionAction      `json:"actions"`
	Approval  *awaitingApprovalTask `json:"approval,omitempty"`
}

// decisionAction is one button rendered in a decision row's action bar.
// Primary marks the row's headline action (rendered with the row's
// stripe colour). Method is "POST" or "GET" — GET is used for the
// "view task" pill which links to the deep page.
type decisionAction struct {
	Label   string `json:"label"`
	Method  string `json:"method"`
	URL     string `json:"url"`
	Primary bool   `json:"primary"`
}

// pulseSnapshot is the data behind the bridge-console pulse-management
// panel on the team-detail page. Derived from rt.pulse + the active
// wake-prompt file. IntervalValue + IntervalUnit feed the number-input
// + select pair so the form posts back the same shape; FormAction is
// pre-built so the template doesn't have to know the URL prefix.
type pulseSnapshot struct {
	Running              bool   `json:"running"`
	Paused               bool   `json:"paused"`
	Interval             string `json:"interval"`       // formatted Go duration ("5m0s")
	IntervalValue        int    `json:"interval_value"` // for the number input
	IntervalUnit         string `json:"interval_unit"`  // "s" / "m" / "h"
	LastTick             string `json:"last_tick"`      // "(never)" or "<duration> ago"
	TickCount            int64  `json:"tick_count"`
	WakePrompt           string `json:"wake_prompt"`             // current value (default or override)
	UseDefaultWakePrompt bool   `json:"use_default_wake_prompt"` // true ⇒ render textarea as placeholder
	DefaultWakePrompt    string `json:"default_wake_prompt"`     // shown as the placeholder text
	StartURL             string `json:"start_url"`               // /control/teams/<id>/pulse/start
	StopURL              string `json:"stop_url"`                // /control/teams/<id>/pulse/stop
	ConfigURL            string `json:"config_url"`              // /control/teams/<id>/pulse/config
}

// workerRow is one entry in the active-workers manifest. Persona is the
// friendly display name from team.PersonaName ("worker-uma" → "Coder
// Uma"); RoleTag is the matching display word ("Coder" / "Reviewer" /
// "Integrator" / "PM"); RoleColourClass is the CSS modifier the
// template appends to .role-tag ("coder" / "reviewer" / "integrator" /
// "planner") so the colour signal stays in CSS, not Go.
type workerRow struct {
	AgentID         string `json:"agent_id"`
	Persona         string `json:"persona"`
	Role            string `json:"role"`
	RoleTag         string `json:"role_tag"`
	RoleColourClass string `json:"role_colour_class"`
	Activity        string `json:"activity"`
	Age             string `json:"age"`
}

// teamHero is the data behind the prominent at-a-glance hero band at
// the top of the team-detail page. ActiveAgentsTotal and OpenTasksTotal
// are large numerals; AgentChips lists every archetype in the team's
// roster (with count, including zero); StageBar enumerates the stages
// that had ≥ 1 task transition today, with proportional segment widths.
type teamHero struct {
	ActiveAgentsTotal int               `json:"active_agents_total"`
	OpenTasksTotal    int               `json:"open_tasks_total"`
	AgentChips        []agentChip       `json:"agent_chips"`
	StageBar          []stageBarSegment `json:"stage_bar"`
	// HasStageActivity is false when no stage had a transition today;
	// the template renders a "no activity today" placeholder.
	HasStageActivity bool `json:"has_stage_activity"`
}

// teamPageBranches wraps the branch list rendered at the bottom of the
// team-detail page. NamePeek is a short comma-joined preview ("teem/a,
// teem/b, teem/c +2 more") shown in the collapsed <summary>; Rows is
// the full table rendered inside the <details> body.
type teamPageBranches struct {
	Count    int               `json:"count"`
	NamePeek string            `json:"name_peek"`
	Rows     []dashboardBranch `json:"rows"`
}

// agentChip is one pill in the per-archetype breakdown above the page
// fold. Count == 0 still renders, so the operator sees the full team
// shape at a glance (predictable layout).
type agentChip struct {
	Role  string `json:"role"`
	Count int    `json:"count"`
}

// stageBarSegment is one coloured segment in the horizontal stacked
// pipeline bar. WidthPct is the relative share of today's task
// transitions (sums to ≤ 100 across segments). TaskIDs is shown as a
// hover tooltip so the operator can spot which task is where.
type stageBarSegment struct {
	Stage    string   `json:"stage"`
	Count    int      `json:"count"`
	WidthPct float64  `json:"width_pct"`
	ColorHex string   `json:"color_hex"`
	TaskIDs  []string `json:"task_ids"`
	// TaskIDList is the space-joined TaskIDs for the title= tooltip
	// (templates can't easily call strings.Join).
	TaskIDList string `json:"task_id_list"`
}

type dashboardAgent struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	State     string `json:"state"`
	LastSeen  string `json:"last_seen"`
	JobsURL   string `json:"jobs_url"`
	Placement string `json:"placement"`
}

type leaderRow struct {
	AgentID        string     `json:"agent_id"`
	Text           string     `json:"text"`
	UpdatedAgo     string     `json:"updated_ago"`
	CurrentTaskIDs []taskLink `json:"current_task_ids"`
}

type taskLink struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// awaitingApprovalTask is the per-row data the team-detail page renders
// inside the "Awaiting approval" section. Carries the bits the operator
// needs to decide: title, id, the leader's notes preview, evidence
// links, and the URLs the inline form posts to.
//
// EvidenceRows is the rich per-evidence-job view (worker, branch,
// touched files); HasPlanArtifact is true when any evidence row is
// plan-shaped (branch only touches docs/**/*.md), which flips the
// card's "Plan artifact" header on. The brief in Notes/NotesPreview
// stays available but the template renders it as a collapsed,
// de-emphasized <details> so the operator's eye lands on the work
// product first.
type awaitingApprovalTask struct {
	ID              string                     `json:"id"`
	Title           string                     `json:"title"`
	Notes           string                     `json:"notes"`
	EvidenceRows    []awaitingApprovalEvidence `json:"evidence_rows"`
	HasPlanArtifact bool                       `json:"has_plan_artifact"`
	StageAgo        string                     `json:"stage_ago"`
	URL             string                     `json:"url"`
	ApproveURL      string                     `json:"approve_url"`
	RejectURL       string                     `json:"reject_url"`
	CommentURL      string                     `json:"comment_url"`
}

type dashboardTask struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	Stage      string `json:"stage"`
	StageAgo   string `json:"stage_ago"`
	AssignedTo string `json:"assigned_to"`
	// Cost carries the per-task token-cost rollup. HasCost is false
	// when pricing.yaml is absent (template hides the cell), or when
	// the task has no priced evidence yet — both render as "—" so the
	// column stays alignment-stable.
	Cost taskCostCell `json:"cost"`
	// AssigneeActive is false when AssignedTo names a worker the
	// registry no longer treats as active (stopped, unregistered, or
	// never seen). The template uses this to mute the cell so it's
	// obvious nobody is currently driving the task.
	AssigneeActive bool `json:"assignee_active"`
	// AssigneeDerived is true when AssignedTo was inferred from the
	// task's latest evidence job (because the task had no explicit
	// assigned_to). The template renders these italicised so the
	// operator can tell explicit assignment from inference.
	AssigneeDerived bool `json:"assignee_derived"`
	// Stale is true when an active pipeline stage (planning/coding/
	// reviewing/integrating) names an inactive assignee — i.e. the task thinks
	// someone is working it but no one is. The template surfaces this
	// as a small STALE pill so the leader knows to re-assign or move
	// the task forward.
	Stale bool   `json:"stale"`
	URL   string `json:"url"`
}

// taskCostCell is the dashboardTask sub-struct holding the rendered
// cost cell + drill-in. Display is the "$X.XX" string the template
// prints; Jobs is the per-evidence breakdown shown inside the
// <details> drawer. Unknown is true when ≥1 contributing event ran on
// a model that pricing.yaml didn't price (UI renders a "?").
type taskCostCell struct {
	HasCost bool          `json:"has_cost"`
	Display string        `json:"display"`
	Unknown bool          `json:"unknown"`
	Jobs    []taskCostJob `json:"jobs"`
}

// taskCostJob is one row in the per-task <details> drill-in: which
// job, which model, how many tokens of each class, and the dollar
// amount that contributed to the task total.
type taskCostJob struct {
	JobID             string `json:"job_id"`
	Model             string `json:"model"`
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CacheCreateTokens int64  `json:"cache_create_tokens"`
	CacheReadTokens   int64  `json:"cache_read_tokens"`
	USD               string `json:"usd"`
	Priced            bool   `json:"priced"`
}

type dashboardEvent struct {
	Time    string `json:"time"`
	AgentID string `json:"agent_id"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	JobID   string `json:"job_id"`
	JobURL  string `json:"job_url"`
}

// renderDashboard composes the summary index — a tile per registered
// team, each linking to /teams/<slug> for the deep view. Designed to
// read at-a-glance across the room: counters in big bold numerals.
func (d *daemon) renderDashboard(w http.ResponseWriter, _ *http.Request) {
	views := d.snapshotTeams()
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })

	state := readDaemonStateFileSafe()

	snap := dashboardSnapshot{
		Endpoint:     d.endpoint,
		StartedAt:    state.StartedAt,
		UptimeAgo:    agoShort(state.StartedAt),
		Tiles:        make([]summaryTile, 0, len(views)),
		NowFormatted: time.Now().Local().Format("Mon Jan 2 15:04:05"),
	}
	for _, v := range views {
		snap.Tiles = append(snap.Tiles, teamTileSnapshot(v))
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
	view := d.snapshotTeam(found)

	state := readDaemonStateFileSafe()
	team := teamSnapshot(view)
	team.Usage = buildUsageSnapshot(d.usageAgg, time.Now())
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
func teamTileSnapshot(v teamView) summaryTile {
	rt := v.rt
	ts := teamSnapshot(v)
	tile := summaryTile{
		Name:                  v.Name,
		Slug:                  rt.team.ID,
		RegisteredAgo:         ts.RegisteredAgo,
		OpenTaskCount:         ts.OpenTaskCount,
		ActiveAgentCount:      len(ts.Agents),
		InFlight:              ts.InFlight,
		PulseRunning:          ts.PulseRunning,
		PulsePaused:           ts.PulsePaused,
		UnreadNotes:           ts.UnreadNotes,
		BranchCount:           ts.Branches.Count,
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
	case "pinged", "busy", "paused", "ping_nudged",
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
func resolvePingOutcome(sink audit.Sink, sinceTS time.Time) (string, string, bool) {
	since := sinceTS.Add(-1 * time.Second)
	events, err := sink.Query("leader", since, 32)
	if err != nil {
		return "", "", false
	}
	for _, ev := range events {
		if ev.Kind != audit.KindPulseTick {
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
func teamSnapshot(v teamView) dashboardTeam {
	rt := v.rt
	out := dashboardTeam{ID: rt.team.ID, Name: v.Name}
	out.RegisteredAgo = agoShort(rt.registered)

	// Pricing: loaded once per render. A missing file flips HasPricing
	// off, which the template uses to hide the Cost column and the
	// hero spend line entirely (per design: "hidden, not $0").
	pricing, pricingOK, _ := usage.LoadPricing(usage.DefaultPricingPath())
	out.HasPricing = pricingOK && pricing.HasPricing()
	out.PricingStale = out.HasPricing && pricing.Stale
	// Cost events scan window: wide enough to cover the 5-most-recent
	// completed tasks (capped at ~24h of activity), narrow enough that
	// the per-render audit scan stays cheap. The TodaysSpend filter
	// then walks this same slice with `since=startOfLocalDay`.
	costEvents := buildCostEvents(rt.auditSink, time.Now().Add(-24*time.Hour))
	if out.HasPricing {
		spend, _ := usage.TodaysSpend(costEvents, startOfLocalDay(time.Now()), pricing)
		out.HeroSpendUSD = spend
		out.HeroSpendDisplay = formatUSD(spend)
	}

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
		// jobLookup maps job_id → agent_id, scanned once per render from
		// the last 72h of audit. Used by taskToDashboardTask to infer an
		// assignee for tasks linked to a job via evidence but never given
		// an explicit assigned_to (e.g. by link_task_to_job).
		jobLookup := buildJobLookup(rt)
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
				out.OpenTasks = append(out.OpenTasks, taskToDashboardTask(rt.team.ID, t, liveAgents, jobLookup, pricing, costEvents))
			case t.Status.IsShelved():
				shelved = append(shelved, t)
			}
		}
		// Awaiting-approval: newest-entered first by StageEnteredAt so a
		// fresh request for signoff sits at the top of the section.
		sort.SliceStable(awaiting, func(i, j int) bool {
			return awaiting[i].StageEnteredAt.After(awaiting[j].StageEnteredAt)
		})
		// Pull one batch of recent audit events for job_id → agent_id
		// resolution across every awaiting card. One query is cheaper
		// than one-per-card; the 7-day window covers multi-round
		// signoffs without dragging in archaeology.
		var evidenceEvents []audit.Event
		if len(awaiting) > 0 && rt.auditSink != nil {
			evidenceEvents, _ = rt.auditSink.Query("", time.Now().Add(-7*24*time.Hour), 5000)
		}
		for _, t := range awaiting {
			out.AwaitingApproval = append(out.AwaitingApproval,
				taskToAwaitingApprovalTask(rt.team.ID, t, evidenceEvents, rt.repoRoot))
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
			out.Shelved = append(out.Shelved, taskToDashboardTask(rt.team.ID, t, liveAgents, jobLookup, pricing, costEvents))
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
			out.RecentDone = append(out.RecentDone, taskToDashboardTask(rt.team.ID, t, liveAgents, jobLookup, pricing, costEvents))
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
		out.Pulse = buildPulseSnapshot(rt)
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

	// Hero: active-agent total + agent chips per archetype + today's
	// stage bar. Computed last so it can read the already-collected
	// counters and the live agent set.
	out.Hero = buildTeamHero(rt, &out)
	out.Workers = buildWorkers(&out)
	out.StatusHeadline = buildStatusHeadline(&out)
	out.Decisions = buildDecisions(rt, &out)

	// Active teem/* branches in the team's working tree. One git
	// invocation per render is fine at v1 scale; if branches counts
	// climb into the hundreds we can layer a small TTL cache here.
	out.HasRepo = rt.repoRoot != ""
	if out.HasRepo {
		rows := listTeemBranches(rt.repoRoot, rt.registry, rt.team.ID)
		out.Branches = teamPageBranches{
			Count:    len(rows),
			NamePeek: branchNamePeek(rows, branchNamePeekLimit),
			Rows:     rows,
		}
	}
	return out
}

// branchNamePeekLimit is the number of branch names rendered inline in
// the collapsed branches <summary> before the remainder is folded into
// "+N more". Chosen so the peek fits comfortably on one row at default
// body font without wrapping.
const branchNamePeekLimit = 5

// branchNamePeek formats the collapsed-branches summary string:
// comma-joined names up to limit, followed by "+N more" when over.
// Returns "" for an empty input so the template can suppress the muted
// span entirely.
func branchNamePeek(rows []dashboardBranch, limit int) string {
	if len(rows) == 0 {
		return ""
	}
	n := len(rows)
	if n <= limit {
		names := make([]string, 0, n)
		for _, r := range rows {
			names = append(names, r.Name)
		}
		return strings.Join(names, ", ")
	}
	names := make([]string, 0, limit)
	for _, r := range rows[:limit] {
		names = append(names, r.Name)
	}
	return strings.Join(names, ", ") + " +" + strconv.Itoa(n-limit) + " more"
}

// stageBarColors maps each canonical stage to the hex colour the hero
// pipeline bar paints its segment with. Kept here (not in CSS) so the
// template can use inline style="background:#…" — segments are
// generated dynamically, so we'd need per-stage classes otherwise.
// AWAITING_APPROVAL is amber to pop ("operator needed"); active stages
// step up the saturation; terminal stages are green / red / grey.
var stageBarColors = map[plan.Stage]string{
	plan.StageProposed:         "#cbd5e1",
	plan.StageSpecced:          "#94a3b8",
	plan.StageAwaitingApproval: "#f59e0b",
	plan.StagePlanning:         "#7dd3fc",
	plan.StageCoding:           "#3b82f6",
	plan.StageReviewing:        "#a78bfa",
	plan.StageIntegrating:      "#fb923c",
	plan.StageVerified:         "#22c55e",
	plan.StageBlocked:          "#ef4444",
	plan.StageShelved:          "#cbd5e1",
}

// decisionStripeColors maps each decision type to its inline-style
// stripe colour, matching the bridge-console palette
// (docs/dashboard-redesign.html): APPROVAL=amber, QUESTION=azure,
// BLOCKER=plum. Kept here so the template can use a single
// style="background:..." attribute per row.
var decisionStripeColors = map[string]string{
	"approval": "#ffb347",
	"question": "#7dd3fc",
	"blocker":  "#a78bfa",
}

// decisionWindowQuestion is how far back the dashboard looks for
// agent-recorded questions (decision_note with severity=question). 24h
// matches the spec — long enough that a question raised overnight
// doesn't fall off before the operator wakes up, short enough that
// week-old conversations don't clutter the panel.
const decisionWindowQuestion = 24 * time.Hour

// decisionWindowBlocker is the lookback for blocker_note events. Wider
// than the question window because an unresolved blocker can sit for
// days; the panel filters out blockers whose task has already left
// stage=blocked, so the longer window is safe.
const decisionWindowBlocker = 7 * 24 * time.Hour

// buildDecisions aggregates the three decision sources into a single
// newest-first slice for the unified Decisions panel:
//
//   - APPROVAL: tasks currently at stage=awaiting_approval (carries the
//     rich evidence/plan-artifact card so the existing operator flow
//     keeps working). Timestamp = task.StageEnteredAt.
//   - QUESTION: most-recent decision_note with severity=question per
//     task in the last 24h. Timestamp = event.Timestamp.
//   - BLOCKER: most-recent blocker_note per task whose task is still
//     at stage=blocked. Timestamp = event.Timestamp.
//
// Returns nil when no decisions are surfaced; the template renders
// "All clear." in that case.
func buildDecisions(rt *registeredTeam, team *dashboardTeam) []decisionRow {
	out := make([]decisionRow, 0, len(team.AwaitingApproval))
	for i := range team.AwaitingApproval {
		a := &team.AwaitingApproval[i]
		row := decisionRow{
			Type:      "APPROVAL",
			TypeClass: "approval",
			TaskID:    a.ID,
			Title:     a.Title,
			Age:       a.StageAgo,
			URL:       a.URL,
			Stripe:    decisionStripeColors["approval"],
			Approval:  a,
			Actions: []decisionAction{
				{Label: "APPROVE", Method: "POST", URL: a.ApproveURL, Primary: true},
				{Label: "REJECT", Method: "POST", URL: a.RejectURL},
				{Label: "COMMENT", Method: "POST", URL: a.CommentURL},
			},
		}
		// StageEnteredAt drives sort order across types; pull it back
		// from the task to keep the ordering correct.
		if rt.plan != nil {
			if t, ok := rt.plan.Get(a.ID); ok {
				row.Timestamp = t.StageEnteredAt
			}
		}
		out = append(out, row)
	}
	if rt.auditSink == nil || rt.plan == nil {
		sortDecisions(out)
		return out
	}
	// One audit scan covers both question + blocker windows; the wider
	// window is the blocker one.
	events, err := rt.auditSink.Query("", time.Now().Add(-decisionWindowBlocker), 5000)
	if err != nil {
		sortDecisions(out)
		return out
	}
	questionCutoff := time.Now().Add(-decisionWindowQuestion)
	seenQuestion := map[string]bool{}
	seenBlocker := map[string]bool{}
	// Iterate newest-first so the seen-map keeps the latest per task.
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		switch e.Kind {
		case audit.KindDecisionNote:
			if e.Timestamp.Before(questionCutoff) {
				continue
			}
			sev, _ := e.Meta["severity"].(string)
			if sev != "question" {
				continue
			}
			taskID, _ := e.Meta["task_id"].(string)
			if taskID == "" || seenQuestion[taskID] {
				continue
			}
			// A later decision_note for the same task (any severity)
			// is the operator's REPLY landing — dismiss the question.
			// events is chronological (oldest first), so indices > i
			// are newer than the question at i.
			answered := false
			for j := i + 1; j < len(events); j++ {
				ne := events[j]
				if ne.Kind != audit.KindDecisionNote {
					continue
				}
				if ntid, _ := ne.Meta["task_id"].(string); ntid == taskID {
					answered = true
					break
				}
			}
			if answered {
				seenQuestion[taskID] = true
				continue
			}
			seenQuestion[taskID] = true
			out = append(out, buildQuestionRow(rt.team.ID, taskID, e, rt.plan))
		case audit.KindBlockerNote:
			taskID, _ := e.Meta["task_id"].(string)
			if taskID == "" || seenBlocker[taskID] {
				continue
			}
			// Skip blockers the operator has already cleared.
			if t, ok := rt.plan.Get(taskID); ok {
				if t.Stage != plan.StageBlocked {
					seenBlocker[taskID] = true
					continue
				}
			} else {
				continue
			}
			seenBlocker[taskID] = true
			out = append(out, buildBlockerRow(rt.team.ID, taskID, e, rt.plan))
		}
	}
	sortDecisions(out)
	return out
}

func sortDecisions(rows []decisionRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].Timestamp.After(rows[j].Timestamp)
	})
}

func buildQuestionRow(teamID, taskID string, e audit.Event, p *plan.Plan) decisionRow {
	title := taskID
	if p != nil {
		if t, ok := p.Get(taskID); ok && t.Title != "" {
			title = t.Title
		}
	}
	base := fmt.Sprintf("/teams/%s/tasks/%s", teamID, taskID)
	row := decisionRow{
		Type:      "QUESTION",
		TypeClass: "question",
		TaskID:    taskID,
		Title:     title,
		Summary:   e.Message,
		Age:       agoShort(e.Timestamp),
		URL:       base,
		Stripe:    decisionStripeColors["question"],
		Timestamp: e.Timestamp,
		Actions: []decisionAction{
			{Label: "REPLY", Method: "POST", URL: fmt.Sprintf("/teams/%s/decisions/%s/reply", teamID, taskID), Primary: true},
		},
	}
	return row
}

func buildBlockerRow(teamID, taskID string, e audit.Event, p *plan.Plan) decisionRow {
	title := taskID
	if p != nil {
		if t, ok := p.Get(taskID); ok && t.Title != "" {
			title = t.Title
		}
	}
	base := fmt.Sprintf("/teams/%s/tasks/%s", teamID, taskID)
	row := decisionRow{
		Type:      "BLOCKER",
		TypeClass: "blocker",
		TaskID:    taskID,
		Title:     title,
		Summary:   e.Message,
		Age:       agoShort(e.Timestamp),
		URL:       base,
		Stripe:    decisionStripeColors["blocker"],
		Timestamp: e.Timestamp,
		Actions: []decisionAction{
			{Label: "UNBLOCK", Method: "POST", URL: fmt.Sprintf("/teams/%s/decisions/%s/unblock", teamID, taskID), Primary: true},
			{Label: "COMMENT", Method: "POST", URL: fmt.Sprintf("/teams/%s/decisions/%s/comment", teamID, taskID)},
		},
	}
	return row
}

// buildTeamHero computes the hero band shown at the top of the
// team-detail page: active-agents total, open-tasks total, one
// alphabetically-sorted chip per archetype declared in the team's
// roster (always including 0-count entries), and a stacked stage bar
// for tasks that entered their current stage today. ABANDONED is
// omitted from the bar entirely (operator-set noise).
func buildTeamHero(rt *registeredTeam, team *dashboardTeam) teamHero {
	h := teamHero{
		ActiveAgentsTotal: len(team.Agents),
		OpenTasksTotal:    team.OpenTaskCount,
	}

	// Agent chips: every archetype from the team YAML, alphabetical,
	// counted from the active-agent set we just collected. Always
	// including zero so the layout is predictable across page loads.
	counts := make(map[string]int, len(rt.team.Archetypes))
	for _, a := range rt.team.Archetypes {
		counts[a.Role] = 0
	}
	for _, da := range team.Agents {
		counts[da.Role]++
	}
	roles := make([]string, 0, len(counts))
	for r := range counts {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	for _, r := range roles {
		h.AgentChips = append(h.AgentChips, agentChip{Role: r, Count: counts[r]})
	}

	// Stage bar: a task is "today" if its current StageEnteredAt is
	// at-or-after local midnight. We can't replay every transition
	// per task (the Plan snapshot keeps only the latest), but for the
	// hero the current-stage-today view answers the operator's
	// question well enough ("which lanes saw movement today?").
	if rt.plan == nil {
		return h
	}
	midnight := startOfLocalDay(time.Now())
	type bucket struct {
		count int
		ids   []string
	}
	buckets := map[plan.Stage]*bucket{}
	total := 0
	for _, t := range rt.plan.List(plan.Filter{}) {
		if t.Stage == plan.StageAbandoned {
			continue
		}
		if t.StageEnteredAt.IsZero() || t.StageEnteredAt.Before(midnight) {
			continue
		}
		b := buckets[t.Stage]
		if b == nil {
			b = &bucket{}
			buckets[t.Stage] = b
		}
		b.count++
		b.ids = append(b.ids, t.ID)
		total++
	}
	if total == 0 {
		return h
	}
	h.HasStageActivity = true
	for _, s := range plan.AllStages {
		b, ok := buckets[s]
		if !ok || b.count == 0 {
			continue
		}
		color := stageBarColors[s]
		if color == "" {
			color = "#cbd5e1"
		}
		h.StageBar = append(h.StageBar, stageBarSegment{
			Stage:      string(s),
			Count:      b.count,
			WidthPct:   100 * float64(b.count) / float64(total),
			ColorHex:   color,
			TaskIDs:    b.ids,
			TaskIDList: strings.Join(b.ids, " "),
		})
	}
	return h
}

// roleColourClasses maps the team's archetype roles to the CSS modifier
// the .role-tag span gets in the workers-panel. Kept in sync with the
// palette the bridge-console mockup (docs/dashboard-redesign.html) uses:
// coder=amber, reviewer=azure, integrator=moss, planner=plum.
var roleColourClasses = map[string]string{
	"worker":          "coder",
	"reviewer":        "reviewer",
	"integrator":      "integrator",
	"project_manager": "planner",
}

// roleDisplayTags maps the team's archetype roles to the short uppercase
// display word the .role-tag span renders. Mirrors the persona logic in
// internal/team.PersonaName so the worker line reads "Coder Uma — CODER"
// rather than "Coder Uma — worker".
var roleDisplayTags = map[string]string{
	"worker":          "CODER",
	"reviewer":        "REVIEWER",
	"integrator":      "INTEGRATOR",
	"project_manager": "PM",
}

// buildPulseSnapshot derives the data the pulse-management panel
// renders. Splits the rounded interval into a number+unit pair so the
// dashboard's number-input + select stay in sync with the running
// pulse, and pre-builds the form-action URLs so the template doesn't
// have to repeat the team-id prefix.
func buildPulseSnapshot(rt *registeredTeam) pulseSnapshot {
	if rt == nil || rt.pulse == nil {
		return pulseSnapshot{}
	}
	wp := rt.pulse.WakePrompt()
	custom := rt.pulse.IsCustomWakePrompt()
	val, unit := splitInterval(rt.pulse.Interval())
	last := "(never)"
	if t := rt.pulse.LastTick(); !t.IsZero() {
		last = agoShort(t)
	}
	base := "/control/teams/" + rt.team.ID + "/pulse"
	return pulseSnapshot{
		Running:              rt.pulse.Running(),
		Paused:               rt.pulse.Paused(),
		Interval:             rt.pulse.Interval().String(),
		IntervalValue:        val,
		IntervalUnit:         unit,
		LastTick:             last,
		TickCount:            rt.pulse.TickCount(),
		WakePrompt:           wp,
		UseDefaultWakePrompt: !custom,
		DefaultWakePrompt:    pulse.DefaultWakePrompt(),
		StartURL:             base + "/start",
		StopURL:              base + "/stop",
		ConfigURL:            base + "/config",
	}
}

// splitInterval picks the largest unit (h/m/s) that the duration is an
// exact multiple of, falling back to seconds for anything sub-minute.
// Used to populate the dashboard's number-input + unit-select from a
// running pulse without lossy conversion.
func splitInterval(d time.Duration) (int, string) {
	if d <= 0 {
		return 5, "m"
	}
	if d%time.Hour == 0 {
		return int(d / time.Hour), "h"
	}
	if d%time.Minute == 0 {
		return int(d / time.Minute), "m"
	}
	return int(d / time.Second), "s"
}

// buildWorkers shapes the active-agent list into the bridge-console
// "Active workers" manifest. One row per agent already collected onto
// team.Agents (so the filtering rules — hide stopped — match the rest
// of the page). Activity prefers the agent's leader-status entry when
// one exists (operator-visible signal), then falls back to the first
// open task assigned to that agent. Empty otherwise.
func buildWorkers(team *dashboardTeam) []workerRow {
	if len(team.Agents) == 0 {
		return nil
	}
	statusByAgent := map[string]string{}
	for _, s := range team.OtherStatuses {
		if s.AgentID != "" && s.Text != "" {
			statusByAgent[s.AgentID] = s.Text
		}
	}
	taskByAgent := map[string]string{}
	for _, t := range team.OpenTasks {
		if t.AssignedTo == "" {
			continue
		}
		if _, seen := taskByAgent[t.AssignedTo]; seen {
			continue
		}
		title := t.Title
		if title == "" {
			title = t.ID
		}
		taskByAgent[t.AssignedTo] = title
	}
	rows := make([]workerRow, 0, len(team.Agents))
	for _, a := range team.Agents {
		tag, ok := roleDisplayTags[a.Role]
		if !ok {
			tag = strings.ToUpper(a.Role)
		}
		colour, ok := roleColourClasses[a.Role]
		if !ok {
			colour = "coder" // unknown roles still get a tag — fall back to amber
		}
		row := workerRow{
			AgentID:         a.ID,
			Persona:         personaOrFallback(a.ID),
			Role:            a.Role,
			RoleTag:         tag,
			RoleColourClass: colour,
			Age:             a.LastSeen,
		}
		switch {
		case statusByAgent[a.ID] != "":
			row.Activity = statusByAgent[a.ID]
		case taskByAgent[a.ID] != "":
			row.Activity = taskByAgent[a.ID]
		}
		rows = append(rows, row)
	}
	return rows
}

// personaOrFallback runs team.PersonaName and falls back to the raw id
// if the result is empty (defensive — PersonaName already returns the
// input unchanged on miss). Centralised so future renderers don't
// reach into the team package for this one transform.
func personaOrFallback(id string) string {
	if p := team.PersonaName(id); p != "" {
		return p
	}
	return id
}

// buildStatusHeadline picks the editorial line shown in the bridge-
// console status panel: today's leader-status text if posted, else a
// quiet-day placeholder. Long lines are clamped to ~200 chars so the
// hero stays one row tall; truncateForTile is reused (same rune-safe
// trim).
func buildStatusHeadline(team *dashboardTeam) string {
	if team.LeaderStatus != nil && team.LeaderStatus.Text != "" {
		return truncateForTile(team.LeaderStatus.Text, 200)
	}
	return "All quiet on the bridge — leader hasn't posted a status yet."
}

// buildUsageSnapshot turns the daemon-global usage.Aggregator into the
// per-team-page Usage card payload. Nil aggregator returns nil (card
// suppressed). When daily_token_budget == 0 the snapshot is marked
// Configured=false so the template renders the configuration hint
// instead of the progress bar.
//
// PerModel rows are sorted by Total descending so the loudest model
// reads first; CacheRead is reported separately because it isn't part
// of the billable total the throttle gates on.
func buildUsageSnapshot(agg *usage.Aggregator, now time.Time) *usageSnapshot {
	if agg == nil {
		return nil
	}
	cfg := agg.Config()
	snap := agg.Snapshot()
	used, capLimit, throttle, _ := agg.AvailableQuota(now)

	out := &usageSnapshot{
		Configured: cfg.DailyTokenBudget > 0,
		Used:       used,
		Cap:        capLimit,
		Throttle:   throttle,
		LastReset:  snap.LastReset,
	}
	if !snap.LastReset.IsZero() {
		out.LastResetAbs = snap.LastReset.Local().Format("Mon Jan 2 15:04 MST")
	}
	if next, err := cfg.NextReset(now); err == nil {
		out.NextReset = next
		until := next.Sub(now)
		out.NextResetIn = formatUntil(until)
		out.NextResetAbs = next.Local().Format("Mon Jan 2 15:04 MST")
	}
	if out.Configured {
		pct := 100 * float64(used) / float64(capLimit)
		if pct < 0 {
			pct = 0
		}
		out.PercentUsed = pct
		out.BarColour = usageBarColour(pct)
	}
	for model, t := range snap.ByModel {
		out.PerModel = append(out.PerModel, modelUsage{
			Model:       model,
			Input:       t.Input,
			Output:      t.Output,
			CacheCreate: t.CacheCreate,
			CacheRead:   t.CacheRead,
			Total:       t.Input + t.Output + t.CacheCreate,
		})
	}
	sort.Slice(out.PerModel, func(i, j int) bool {
		if out.PerModel[i].Total != out.PerModel[j].Total {
			return out.PerModel[i].Total > out.PerModel[j].Total
		}
		return out.PerModel[i].Model < out.PerModel[j].Model
	})
	return out
}

// usageBarColour picks the progress-bar shade by percent-used. The
// breakpoints match the spec (green <50, amber 50-80, red ≥80); 80% is
// the default throttle threshold so the visual cue lines up with the
// gate.
func usageBarColour(pct float64) string {
	switch {
	case pct >= 80:
		return "red"
	case pct >= 50:
		return "amber"
	default:
		return "green"
	}
}

// formatUntil renders a future duration as "4h 23m" / "23m" / "now".
// Sub-minute precision is dropped because the operator never needs it
// for a daily-reset countdown.
func formatUntil(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// taskToDashboardTask converts a plan.Task to the row shape rendered
// by the dashboard template. liveAgents is the set of currently active
// (non-stopped) agent ids; it's used to decide whether the task's
// AssignedTo is being actively worked or pointing at a worker that
// has gone away — the latter is rendered muted and flagged STALE when
// the stage is one where someone should be holding the task.
//
// jobLookup resolves a job_id to its owning agent_id; it's used to
// derive an assignee for tasks that were linked to a job via evidence
// but never had assigned_to set explicitly (link_task_to_job is
// evidence-only by design).
func taskToDashboardTask(team string, t plan.Task, liveAgents map[string]bool, jobLookup func(jobID string) (string, bool), pricing usage.Pricing, costEvents []usage.CostEvent) dashboardTask {
	stageAgo := ""
	if !t.StageEnteredAt.IsZero() {
		stageAgo = agoShort(t.StageEnteredAt)
	}
	assignee, derived := deriveAssignee(t, jobLookup)
	assigneeActive := assignee == "" || liveAgents[assignee]
	stale := false
	if assignee != "" && !assigneeActive {
		switch t.Stage {
		case plan.StagePlanning, plan.StageCoding, plan.StageReviewing, plan.StageIntegrating:
			stale = true
		}
	}
	return dashboardTask{
		ID:              t.ID,
		Title:           t.Title,
		Status:          string(t.Status),
		Stage:           string(t.Stage),
		StageAgo:        stageAgo,
		AssignedTo:      assignee,
		AssigneeActive:  assigneeActive,
		AssigneeDerived: derived,
		Stale:           stale,
		URL:             fmt.Sprintf("/teams/%s/tasks/%s", team, t.ID),
		Cost:            buildTaskCostCell(t.Evidence, pricing, costEvents),
	}
}

// buildTaskCostCell turns the per-task PerTaskCost result into the
// view-ready taskCostCell. HasCost stays false when pricing isn't
// loaded (template hides the cell) and when the task has no priced
// evidence (template still hides — operators don't want an empty $0
// in the column).
func buildTaskCostCell(evidence []string, pricing usage.Pricing, costEvents []usage.CostEvent) taskCostCell {
	cb, ok := usage.PerTaskCost(costEvents, evidence, pricing)
	if !ok {
		return taskCostCell{}
	}
	if len(cb.Jobs) == 0 {
		return taskCostCell{}
	}
	cell := taskCostCell{
		HasCost: true,
		Display: formatUSD(cb.USD),
		Unknown: cb.UnknownModels,
		Jobs:    make([]taskCostJob, 0, len(cb.Jobs)),
	}
	for _, j := range cb.Jobs {
		row := taskCostJob{
			JobID:             j.JobID,
			Model:             j.Model,
			InputTokens:       j.InputTokens,
			OutputTokens:      j.OutputTokens,
			CacheCreateTokens: j.CacheCreateTokens,
			CacheReadTokens:   j.CacheReadTokens,
			Priced:            j.Priced,
		}
		if j.Priced {
			row.USD = formatUSD(j.USD)
		} else {
			row.USD = "—"
		}
		cell.Jobs = append(cell.Jobs, row)
	}
	return cell
}

// deriveAssignee resolves the assignee for a task. Explicit
// assigned_to wins; otherwise the task's evidence list is walked
// newest-first and the first job whose agent_id can be looked up is
// returned with derived=true so the renderer can mark it as inferred.
// Returns ("", false) when nothing resolves.
func deriveAssignee(t plan.Task, jobLookup func(jobID string) (string, bool)) (string, bool) {
	if t.AssignedTo != "" {
		return t.AssignedTo, false
	}
	if jobLookup == nil {
		return "", false
	}
	for i := len(t.Evidence) - 1; i >= 0; i-- {
		if a, ok := jobLookup(t.Evidence[i]); ok && a != "" {
			return a, true
		}
	}
	return "", false
}

// buildJobLookup scans the last 72h of audit events for the team and
// returns a job_id → agent_id resolver. One scan per page render is
// cheap enough at v1 scale and matches the window used by the per-agent
// jobs page.
func buildJobLookup(rt *registeredTeam) func(string) (string, bool) {
	empty := func(string) (string, bool) { return "", false }
	if rt.auditSink == nil {
		return empty
	}
	events, err := rt.auditSink.Query("", time.Now().Add(-72*time.Hour), 5000)
	if err != nil || len(events) == 0 {
		return empty
	}
	jobs := audit.MaterializeJobs(events)
	m := make(map[string]string, len(jobs))
	for _, j := range jobs {
		if j.AgentID != "" {
			m[j.JobID] = j.AgentID
		}
	}
	return func(jobID string) (string, bool) {
		a, ok := m[jobID]
		return a, ok
	}
}

// taskToAwaitingApprovalTask packages a plan.Task in awaiting_approval
// stage for the dashboard's prominent "Awaiting approval" section,
// pre-baking the form action URLs, a clamped notes preview, and the
// resolved evidence rows (worker → branch → changed files). The
// audit event slice is consulted to map each job_id to its
// originating agent_id; repoRoot drives the per-branch file listing.
func taskToAwaitingApprovalTask(team string, t plan.Task, events []audit.Event, repoRoot string) awaitingApprovalTask {
	stageAgo := ""
	if !t.StageEnteredAt.IsZero() {
		stageAgo = agoShort(t.StageEnteredAt)
	}
	base := fmt.Sprintf("/teams/%s/tasks/%s", team, t.ID)
	rows := resolveEvidenceRows(events, t.Evidence, repoRoot, team)
	hasPlan := false
	for _, r := range rows {
		if r.PlanShaped {
			hasPlan = true
			break
		}
	}
	return awaitingApprovalTask{
		ID:              t.ID,
		Title:           t.Title,
		Notes:           t.Notes,
		EvidenceRows:    rows,
		HasPlanArtifact: hasPlan,
		StageAgo:        stageAgo,
		URL:             base,
		ApproveURL:      base + "/approve",
		RejectURL:       base + "/reject",
		CommentURL:      base + "/comment",
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
