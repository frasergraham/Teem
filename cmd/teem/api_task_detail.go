package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/plan"
)

// apiTaskDetailPayload is the JSON returned by GET
// /api/teams/<id>/tasks/<task-id>. Joins three views over the same
// task:
//
//   - Task is the plan-side record (title, stage, notes, evidence,
//     stamps).
//   - Timeline is every audit event in this team's sink that carries
//     meta.task_id == <task-id>, plus every event tagged with one of
//     the task's evidence job_ids. Newest first.
//   - Agents is a per-agent rollup over the evidence jobs, so the
//     modal can show "ada: 2 jobs, last 2h ago, done" at a glance.
//   - Jobs is the per-evidence MaterializedJob, each with the URL the
//     SPA hands the operator to download the raw NDJSON.
//
// Auth model matches /api/teams/<id>/state: tailnet boundary, no
// bearer required. The modal is read-only.
type apiTaskDetailPayload struct {
	Now      time.Time              `json:"now"`
	Task     apiTaskRecord          `json:"task"`
	Timeline []apiTaskTimelineEvent `json:"timeline"`
	Agents   []apiTaskAgentRollup   `json:"agents"`
	Jobs     []apiTaskJob           `json:"jobs"`
}

// apiTaskRecord is the plan.Task projection. Times are preserved as
// RFC3339 so the SPA can format them client-side; agoShort renders
// are kept for the modal-header tiles.
type apiTaskRecord struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	Stage      string `json:"stage"`
	AssignedTo string `json:"assigned_to,omitempty"`
	Notes      string `json:"notes,omitempty"`
	// Origin is the snapshotted plan.Task.Origin (operator|leader|
	// project_manager|system). Legacy tasks default to "operator" at
	// replay; surface it so the SPA can render the synthetic creation
	// row at the head of the participation log.
	Origin         string    `json:"origin,omitempty"`
	ParentID       string    `json:"parent_id,omitempty"`
	Evidence       []string  `json:"evidence,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	UpdatedAgo     string    `json:"updated_ago"`
	StageEnteredAt time.Time `json:"stage_entered_at,omitempty"`
	StageAgo       string    `json:"stage_ago,omitempty"`
}

// apiTaskTimelineEvent is one row in the audit timeline. Source is
// either "task" (event was tagged with meta.task_id) or "job" (event
// belonged to an evidence job); the SPA may use that to badge rows.
// Summary is a server-rendered, human-readable line composed from
// the event's kind + meta; the SPA prefers it over Message for kinds
// (notably task_stage_changed) whose Message is empty.
type apiTaskTimelineEvent struct {
	Timestamp time.Time      `json:"ts"`
	Kind      string         `json:"kind"`
	AgentID   string         `json:"agent_id,omitempty"`
	JobID     string         `json:"job_id,omitempty"`
	Message   string         `json:"message,omitempty"`
	Summary   string         `json:"summary,omitempty"`
	Source    string         `json:"source"`
	Meta      map[string]any `json:"meta,omitempty"`
}

// apiTaskAgentRollup summarises one agent's contribution to the task:
// how many evidence jobs they own, how those jobs ended, and when
// they last touched the task. FirstSeenAt is the earliest job start,
// LastSeenAt the latest job end (or start if still pending).
type apiTaskAgentRollup struct {
	AgentID     string    `json:"agent_id"`
	JobCount    int       `json:"job_count"`
	Done        int       `json:"done"`
	Errored     int       `json:"errored"`
	Pending     int       `json:"pending"`
	FirstSeenAt time.Time `json:"first_seen_at,omitempty"`
	LastSeenAt  time.Time `json:"last_seen_at,omitempty"`
	LastSeenAgo string    `json:"last_seen_ago,omitempty"`
}

// apiTaskJob is one materialized evidence job. TranscriptURL is the
// SPA-visible link to the rendered transcript page
// (/teams/<id>/transcripts/<agent>/<job>); populated when a
// transcript event has been recorded (TranscriptBytes > 0).
type apiTaskJob struct {
	JobID           string    `json:"job_id"`
	AgentID         string    `json:"agent_id,omitempty"`
	Status          string    `json:"status"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	CompletedAt     time.Time `json:"completed_at,omitempty"`
	DurationMs      int64     `json:"duration_ms,omitempty"`
	Summary         string    `json:"summary,omitempty"`
	TranscriptBytes int       `json:"transcript_bytes,omitempty"`
	TranscriptURL   string    `json:"transcript_url,omitempty"`
}

// handleAPITeamTaskDetail serves GET /api/teams/<id>/tasks/<task-id>.
// Returns 404 if the team or task is unknown; 500 on plan/audit
// errors. Tasks with no evidence and no audit-tagged events return a
// well-formed payload with empty Timeline/Agents/Jobs — the SPA shows
// the plan record and a "no audit activity yet" hint.
func (d *daemon) handleAPITeamTaskDetail(w http.ResponseWriter, _ *http.Request, rt *registeredTeam, taskID string) {
	if !isSafeID(taskID) {
		http.Error(w, "bad task id", http.StatusBadRequest)
		return
	}
	if rt.plan == nil {
		http.Error(w, "plan unavailable", http.StatusInternalServerError)
		return
	}
	task, ok := rt.plan.Get(taskID)
	if !ok {
		http.NotFound(w, nil)
		return
	}

	now := time.Now().UTC()
	payload := apiTaskDetailPayload{
		Now:      now,
		Task:     toAPITaskRecord(task),
		Timeline: []apiTaskTimelineEvent{},
		Agents:   []apiTaskAgentRollup{},
		Jobs:     []apiTaskJob{},
	}

	if rt.auditSink != nil {
		events, err := rt.auditSink.Query("", time.Time{}, 0)
		if err == nil {
			payload.Timeline = buildTaskTimeline(task, events)
			payload.Jobs = buildTaskJobs(task, events, rt.team.ID)
			payload.Agents = buildTaskAgentRollups(payload.Jobs, payload.Timeline)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(payload)
}

func toAPITaskRecord(t plan.Task) apiTaskRecord {
	rec := apiTaskRecord{
		ID:         t.ID,
		Title:      t.Title,
		Status:     string(t.Status),
		Stage:      string(t.Stage),
		AssignedTo: t.AssignedTo,
		Notes:      t.Notes,
		Origin:     string(t.Origin),
		ParentID:   t.ParentID,
		Evidence:   append([]string(nil), t.Evidence...),
		CreatedAt:  t.CreatedAt,
		UpdatedAt:  t.UpdatedAt,
	}
	if !t.UpdatedAt.IsZero() {
		rec.UpdatedAgo = agoShort(t.UpdatedAt)
	}
	if !t.StageEnteredAt.IsZero() {
		rec.StageEnteredAt = t.StageEnteredAt
		rec.StageAgo = agoShort(t.StageEnteredAt)
	}
	return rec
}

// buildTaskTimeline returns the audit events relevant to this task:
// anything tagged with meta.task_id == task.ID (source="task") plus
// every event whose JobID is in the task's Evidence (source="job").
// Sorted newest first.
func buildTaskTimeline(task plan.Task, events []audit.Event) []apiTaskTimelineEvent {
	evidence := make(map[string]bool, len(task.Evidence))
	for _, jid := range task.Evidence {
		evidence[jid] = true
	}
	out := make([]apiTaskTimelineEvent, 0)
	for _, e := range events {
		matchedByTask := false
		if id, ok := e.Meta["task_id"].(string); ok && id == task.ID {
			matchedByTask = true
		}
		matchedByJob := e.JobID != "" && evidence[e.JobID]
		if !matchedByTask && !matchedByJob {
			continue
		}
		source := "job"
		if matchedByTask {
			source = "task"
		}
		out = append(out, apiTaskTimelineEvent{
			Timestamp: e.Timestamp,
			Kind:      string(e.Kind),
			AgentID:   e.AgentID,
			JobID:     e.JobID,
			Message:   e.Message,
			Summary:   summarizeTaskEvent(e),
			Source:    source,
			Meta:      e.Meta,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.After(out[j].Timestamp)
	})
	return out
}

// buildTaskJobs materialises every job referenced by task.Evidence.
// Jobs the audit log no longer remembers (pruned) appear as stubs
// with Status="unknown" so the SPA can still surface the job-id.
// transcript_url is the unauth NDJSON path served by
// handleAPITeamTranscriptGet; only populated when the transcript-ready
// event has fired.
func buildTaskJobs(task plan.Task, events []audit.Event, teamID string) []apiTaskJob {
	if len(task.Evidence) == 0 {
		return []apiTaskJob{}
	}
	all := audit.MaterializeJobs(events)
	byID := make(map[string]audit.MaterializedJob, len(all))
	for _, j := range all {
		byID[j.JobID] = j
	}
	out := make([]apiTaskJob, 0, len(task.Evidence))
	for _, jid := range task.Evidence {
		j, ok := byID[jid]
		if !ok {
			out = append(out, apiTaskJob{JobID: jid, Status: "unknown"})
			continue
		}
		row := apiTaskJob{
			JobID:           j.JobID,
			AgentID:         j.AgentID,
			Status:          j.Status,
			StartedAt:       j.StartedAt,
			CompletedAt:     j.CompletedAt,
			Summary:         j.Summary,
			TranscriptBytes: j.TranscriptBytes,
		}
		if d := j.Duration(); d > 0 {
			row.DurationMs = d.Milliseconds()
		}
		if j.TranscriptBytes > 0 && j.AgentID != "" && isSafeID(j.AgentID) && isSafeID(j.JobID) {
			// SPA path — opens <TranscriptPage>, which fetches the raw
			// NDJSON from /api/teams/<id>/transcripts/<a>/<j> itself.
			row.TranscriptURL = fmt.Sprintf("/teams/%s/transcripts/%s/%s", teamID, j.AgentID, j.JobID)
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

// buildTaskAgentRollups groups jobs by agent and counts status
// buckets. Sort: most recently active first (descending LastSeenAt),
// ties broken alphabetically by agent_id so the order is stable.
//
// Two sources are unioned:
//
//  1. Evidence jobs (the primary source). Every materialised job
//     contributes one JobCount + a Done/Errored/Pending tick and
//     stretches the agent's first/last seen window.
//  2. Timeline events tagged with meta.task_id == task.ID
//     (the belt-and-braces source). Worker events whose JobID never
//     made it onto task.Evidence — e.g. an audit row that landed
//     before plan.LinkJob committed, or one whose job_id was
//     pruned — still surface their agent in the rollup. JobCount is
//     left alone (we have no way to tell jobs apart from raw events
//     without re-deriving MaterializedJobs from scratch) but the
//     time bracket and agent itself are recorded so the operator
//     sees who participated.
func buildTaskAgentRollups(jobs []apiTaskJob, timeline []apiTaskTimelineEvent) []apiTaskAgentRollup {
	byAgent := map[string]*apiTaskAgentRollup{}
	seenJobs := map[string]bool{}
	for _, j := range jobs {
		if j.AgentID == "" {
			continue
		}
		seenJobs[j.JobID] = true
		r, ok := byAgent[j.AgentID]
		if !ok {
			r = &apiTaskAgentRollup{AgentID: j.AgentID}
			byAgent[j.AgentID] = r
		}
		r.JobCount++
		switch j.Status {
		case "done":
			r.Done++
		case "error":
			r.Errored++
		default:
			r.Pending++
		}
		if !j.StartedAt.IsZero() {
			if r.FirstSeenAt.IsZero() || j.StartedAt.Before(r.FirstSeenAt) {
				r.FirstSeenAt = j.StartedAt
			}
		}
		// LastSeenAt is the latest of CompletedAt (for finished jobs)
		// and StartedAt (everything else).
		last := j.CompletedAt
		if last.IsZero() {
			last = j.StartedAt
		}
		if !last.IsZero() && last.After(r.LastSeenAt) {
			r.LastSeenAt = last
		}
	}
	// Union pass: any timeline event tagged with the task that
	// references an agent we haven't already accounted for via
	// Evidence stretches the agent's seen window. Skips events whose
	// job_id was already counted to avoid double-counting on a
	// fully-linked task.
	for _, e := range timeline {
		if e.AgentID == "" {
			continue
		}
		if e.JobID != "" && seenJobs[e.JobID] {
			continue
		}
		r, ok := byAgent[e.AgentID]
		if !ok {
			r = &apiTaskAgentRollup{AgentID: e.AgentID}
			byAgent[e.AgentID] = r
		}
		if r.FirstSeenAt.IsZero() || e.Timestamp.Before(r.FirstSeenAt) {
			r.FirstSeenAt = e.Timestamp
		}
		if e.Timestamp.After(r.LastSeenAt) {
			r.LastSeenAt = e.Timestamp
		}
	}
	if len(byAgent) == 0 {
		return []apiTaskAgentRollup{}
	}
	out := make([]apiTaskAgentRollup, 0, len(byAgent))
	for _, r := range byAgent {
		if !r.LastSeenAt.IsZero() {
			r.LastSeenAgo = agoShort(r.LastSeenAt)
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSeenAt.Equal(out[j].LastSeenAt) {
			return out[i].LastSeenAt.After(out[j].LastSeenAt)
		}
		return out[i].AgentID < out[j].AgentID
	})
	return out
}

// summarizeTaskEvent renders a one-line human-readable description of
// an audit event for the task-detail modal's timeline. Kind-aware so
// the operator sees "stage proposed → ready" rather than a bare
// "task_stage_changed". Falls back to Message, then the kind name, so
// no row ever renders as empty text.
func summarizeTaskEvent(e audit.Event) string {
	switch e.Kind {
	case audit.KindTaskStageChanged:
		from, _ := e.Meta["from"].(string)
		to, _ := e.Meta["to"].(string)
		if from != "" && to != "" {
			return fmt.Sprintf("stage %s → %s", from, to)
		}
		if to != "" {
			return fmt.Sprintf("stage → %s", to)
		}
		if e.Message != "" {
			return e.Message
		}
		return string(e.Kind)
	case audit.KindDecisionNote, audit.KindBlockerNote:
		if e.Message != "" {
			return e.Message
		}
		return string(e.Kind)
	case audit.KindJobReceived:
		return fmt.Sprintf("%s started job %s", e.AgentID, shortJobID(e.JobID))
	case audit.KindJobComplete:
		dur := metaDurationStr(e.Meta)
		calls, hasCalls := metaToolCalls(e.Meta)
		head := fmt.Sprintf("%s finished job %s", e.AgentID, shortJobID(e.JobID))
		switch {
		case dur != "" && hasCalls:
			return fmt.Sprintf("%s (%s, %d tool calls)", head, dur, calls)
		case dur != "":
			return fmt.Sprintf("%s (%s)", head, dur)
		case hasCalls:
			return fmt.Sprintf("%s (%d tool calls)", head, calls)
		default:
			return head
		}
	case audit.KindJobError:
		msg, _ := e.Meta["error"].(string)
		if msg == "" {
			msg = e.Message
		}
		head := fmt.Sprintf("%s errored on job %s", e.AgentID, shortJobID(e.JobID))
		if msg != "" {
			return fmt.Sprintf("%s: %s", head, msg)
		}
		return head
	case audit.KindJobInterrupted:
		return fmt.Sprintf("%s interrupted on job %s", e.AgentID, shortJobID(e.JobID))
	case audit.KindJobTranscriptReady:
		return fmt.Sprintf("transcript ready for job %s", shortJobID(e.JobID))
	case audit.KindWorkerStopped:
		return fmt.Sprintf("%s stopped", e.AgentID)
	}
	if e.Message != "" {
		return e.Message
	}
	return string(e.Kind)
}

// shortJobID truncates a job_id to its first 8 chars for inline use.
// Most job IDs are 16+ hex; if the id is already shorter we return it
// unchanged. Empty input → empty output (caller handles the "no job"
// case).
func shortJobID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// metaDurationStr returns durShort(duration_ms) or "" if the meta
// key is missing/unparseable. JSON round-trip turns ints into float64
// so we accept both.
func metaDurationStr(m map[string]any) string {
	if m == nil {
		return ""
	}
	var ms int64
	switch v := m["duration_ms"].(type) {
	case float64:
		ms = int64(v)
	case int:
		ms = int64(v)
	case int64:
		ms = v
	default:
		return ""
	}
	if ms <= 0 {
		return ""
	}
	return durShort(time.Duration(ms) * time.Millisecond)
}

// metaToolCalls returns (count, ok); ok=false when the key is absent
// or unparseable. Accepts float64 (JSON path) and int (in-memory).
func metaToolCalls(m map[string]any) (int, bool) {
	if m == nil {
		return 0, false
	}
	switch v := m["tool_calls"].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	}
	return 0, false
}
