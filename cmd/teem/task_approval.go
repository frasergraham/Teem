package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/plan"
)

// decision is one of approve / reject / comment — the operator action
// taken against a task sitting in awaiting_approval.
type decision string

const (
	decisionApprove decision = "approve"
	decisionReject  decision = "reject"
	decisionComment decision = "comment"
)

// errReadyFromTerminal is returned by markTaskReady when the caller
// tries to flip a verified or abandoned task into ready. Mapped to
// HTTP 409 by the route handler; reusing the stage-conflict status
// since the task is in a stage incompatible with the operator's
// "pre-flighted, dispatch this" signal.
var errReadyFromTerminal = errors.New("task is terminal (verified/abandoned); cannot mark ready")

// errNotAwaitingApproval is returned by decideTask when the caller
// tries to approve/reject/comment on a task whose Stage isn't
// awaiting_approval. Mapped to HTTP 409 by the route handlers.
var errNotAwaitingApproval = errors.New("task is not in awaiting_approval")

// errStageRaced is returned by decideTask when a concurrent decision
// moved the task out of awaiting_approval between the pre-check and
// the conditional write. Distinct from errNotAwaitingApproval so the
// operator-facing message can suggest a refresh-and-retry rather than
// hinting the task was never up for review. Mapped to HTTP 409.
var errStageRaced = errors.New("task stage changed since check; refresh and retry")

// decideTask is the shared core for approve / reject / comment. It
// validates the task is in awaiting_approval, mutates the plan
// (stage transition + notes append), and writes a decision_note
// audit event; operator-facing notification (channel push, etc.)
// is delivered by the hookedSink fan-out on that audit Write.
//
// Returns the updated task on success.
//
// Errors: plan.ErrTaskNotFound (caller maps to 404),
// errNotAwaitingApproval (caller maps to 409), plus underlying plan
// write errors (caller maps to 500).
func decideTask(rt *registeredTeam, taskID string, d decision, comment string) (plan.Task, error) {
	if rt == nil || rt.plan == nil {
		return plan.Task{}, errors.New("plan unavailable")
	}
	switch d {
	case decisionApprove, decisionReject, decisionComment:
	default:
		return plan.Task{}, fmt.Errorf("unknown decision: %q", d)
	}
	current, ok := rt.plan.Get(taskID)
	if !ok {
		return plan.Task{}, plan.ErrTaskNotFound
	}
	if current.Stage != plan.StageAwaitingApproval {
		return plan.Task{}, errNotAwaitingApproval
	}

	// MutateTaskIfStage holds the plan mutex across the stage check,
	// the read of the live task, and the write. Without the in-lock read
	// of t.Notes, two concurrent decisions (esp. COMMENT+COMMENT, where
	// stage doesn't change) can both build newNotes from the pre-race
	// snapshot and the second write clobbers the first's appended line.
	updated, err := rt.plan.MutateTaskIfStage(taskID, plan.StageAwaitingApproval, func(t plan.Task) plan.UpdateInput {
		newNotes := appendDecisionNote(t.Notes, d, comment)
		in := plan.UpdateInput{Notes: &newNotes}
		switch d {
		case decisionApprove:
			in.Stage = plan.StageCoding
		case decisionReject:
			in.Stage = plan.StageShelved
			in.Status = plan.StatusShelved
		case decisionComment:
			// Stay in awaiting_approval — self-transition is allowed by
			// the matrix; UpdateTask leaves StageEnteredAt alone since
			// the stage didn't actually change.
			in.Stage = plan.StageAwaitingApproval
		}
		return in
	})
	if errors.Is(err, plan.ErrStageChanged) {
		return plan.Task{}, errStageRaced
	}
	if err != nil {
		return plan.Task{}, err
	}

	// Audit: record_decision-style entry, agent_id=operator since the
	// dashboard / control endpoints represent the human operator.
	if rt.auditSink != nil {
		_ = rt.auditSink.Write(audit.Event{
			Timestamp: time.Now().UTC(),
			AgentID:   "operator",
			Kind:      audit.KindDecisionNote,
			Message:   formatDecisionMessage(d, comment),
			Meta: map[string]any{
				"task_id":  taskID,
				"decision": string(d),
				"comment":  comment,
			},
		})
	}

	// The hooked audit sink runs channelHook on every Write, so the
	// decision_note above already fans out to any connected leader
	// session — no explicit PushChannel is needed here.

	return updated, nil
}

// appendDecisionNote returns notes + a new prefixed line for the
// decision. Existing notes are kept verbatim above the new line.
func appendDecisionNote(existing string, d decision, comment string) string {
	stamp := time.Now().UTC().Format("2006-01-02 15:04 UTC")
	prefix := ""
	switch d {
	case decisionApprove:
		prefix = "APPROVED"
	case decisionReject:
		prefix = "REJECTED"
	case decisionComment:
		prefix = "COMMENT"
	}
	line := fmt.Sprintf("[%s %s]", prefix, stamp)
	if comment != "" {
		line += " " + comment
	}
	if existing == "" {
		return line
	}
	return strings.TrimRight(existing, "\n") + "\n\n" + line
}

// formatDecisionMessage builds the human-readable audit Message body.
// The Meta map carries the structured fields; this string is what the
// task-flow page and dashboard event stream render.
func formatDecisionMessage(d decision, comment string) string {
	verb := ""
	switch d {
	case decisionApprove:
		verb = "approved task"
	case decisionReject:
		verb = "rejected task"
	case decisionComment:
		verb = "commented on task"
	}
	if comment == "" {
		return verb
	}
	return verb + ": " + comment
}

// taskActionRequest is the JSON body accepted by the /control/
// endpoints. Both `comment` and `reason` are accepted for ergonomics;
// reject requires a reason, comment requires a comment, approve takes
// either field optionally.
type taskActionRequest struct {
	Comment string `json:"comment"`
	Reason  string `json:"reason"`
}

func (r taskActionRequest) text() string {
	if r.Comment != "" {
		return r.Comment
	}
	return r.Reason
}

// handleControlTaskAction is the JSON API surface for the three
// task-decision endpoints under /control/teams/<id>/tasks/<task_id>/<action>.
// Bearer-auth gated (caller already checked via requireAuth).
//
// Subpath shape (the bit after "tasks/"): "<task_id>/<action>".
func (d *daemon) handleControlTaskAction(w http.ResponseWriter, r *http.Request, rt *registeredTeam, subpath string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	taskID, action, ok := splitTaskActionPath(subpath)
	if !ok {
		http.Error(w, "want tasks/<task_id>/(approve|reject|comment)", http.StatusBadRequest)
		return
	}
	if !isSafeID(taskID) {
		http.Error(w, "bad task id", http.StatusBadRequest)
		return
	}

	var req taskActionRequest
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	r.Body.Close()
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	dec, text, err := resolveActionInput(action, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	task, err := decideTask(rt, taskID, dec, text)
	switch {
	case errors.Is(err, plan.ErrTaskNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, errNotAwaitingApproval), errors.Is(err, errStageRaced):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, task)
}

// resolveActionInput maps the path action verb + body to a (decision,
// text) pair, enforcing the per-action body requirements:
//   - approve: comment optional
//   - reject:  reason required
//   - comment: comment required
//
// Both `comment` and `reason` are accepted on every action, with the
// off-axis field as a fallback. This is a deliberate ergonomic
// shortcut for the dashboard form (ui_dashboard.html), which has a
// single text input that doubles as the optional approve comment AND
// the required reject reason — so a form-submitted "comment" value can
// satisfy a reject's reason requirement.
func resolveActionInput(action string, req taskActionRequest) (decision, string, error) {
	switch action {
	case "approve":
		return decisionApprove, req.text(), nil
	case "reject":
		text := req.Reason
		if text == "" {
			text = req.Comment
		}
		if strings.TrimSpace(text) == "" {
			return "", "", errors.New("reject requires a reason")
		}
		return decisionReject, text, nil
	case "comment":
		text := req.Comment
		if text == "" {
			text = req.Reason
		}
		if strings.TrimSpace(text) == "" {
			return "", "", errors.New("comment requires a comment")
		}
		return decisionComment, text, nil
	}
	return "", "", fmt.Errorf("unknown action: %q (want approve|reject|comment)", action)
}

// markTaskReady flips a task into plan.StageReady — the operator's
// "pre-flighted, free to dispatch" signal. Idempotent on already-ready
// tasks (returns the existing task with changed=false and without
// writing). Returns errReadyFromTerminal when the task is verified or
// abandoned; plan.ErrTaskNotFound when the id is missing;
// plan.ErrInvalidStage (or errStageRaced after retries) when the
// matrix forbids the move from the task's current stage.
//
// The changed bool tells callers whether the stage actually
// transitioned on this call. handleControlTaskReady uses it to decide
// whether to fire the operator auto-wake — idempotent re-posts on an
// already-ready task should not wake the leader again.
//
// Race handling: the operator can double-click the button, and the
// PM loop can also be moving the task. Each attempt locks expected
// = currentStage via MutateTaskIfStage so two concurrent flips can't
// both pass the read-then-write check. On ErrStageChanged we re-read
// and retry up to maxReadyRetries.
const maxReadyRetries = 3

func markTaskReady(rt *registeredTeam, taskID string) (plan.Task, bool, error) {
	if rt == nil || rt.plan == nil {
		return plan.Task{}, false, errors.New("plan unavailable")
	}
	for i := 0; i < maxReadyRetries; i++ {
		current, ok := rt.plan.Get(taskID)
		if !ok {
			return plan.Task{}, false, plan.ErrTaskNotFound
		}
		if current.Stage == plan.StageReady {
			return current, false, nil
		}
		if current.Stage == plan.StageVerified || current.Stage == plan.StageAbandoned {
			return plan.Task{}, false, errReadyFromTerminal
		}
		updated, err := rt.plan.MutateTaskIfStage(taskID, current.Stage, func(plan.Task) plan.UpdateInput {
			return plan.UpdateInput{Stage: plan.StageReady}
		})
		if errors.Is(err, plan.ErrStageChanged) {
			continue
		}
		if err != nil {
			return plan.Task{}, false, err
		}
		if rt.auditSink != nil {
			_ = rt.auditSink.Write(audit.Event{
				Timestamp: time.Now().UTC(),
				AgentID:   "operator",
				Kind:      audit.KindTaskStageChanged,
				Message:   "operator marked task ready",
				Meta: map[string]any{
					"task_id": taskID,
					"from":    string(current.Stage),
					"to":      string(plan.StageReady),
				},
			})
		}
		return updated, true, nil
	}
	return plan.Task{}, false, errStageRaced
}

// handleControlTaskReady is the JSON API surface for POST
// /control/teams/<id>/tasks/<task_id>/ready. Unauth on purpose (tailnet
// boundary, same model as the dashboard's pulse-action POSTs); body is
// ignored.
func (d *daemon) handleControlTaskReady(w http.ResponseWriter, r *http.Request, rt *registeredTeam, taskID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isSafeID(taskID) {
		http.Error(w, "bad task id", http.StatusBadRequest)
		return
	}
	// Drain (but ignore) any body — keeps clients that send `{}` happy.
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64*1024))
	r.Body.Close()

	task, changed, err := markTaskReady(rt, taskID)
	switch {
	case errors.Is(err, plan.ErrTaskNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, errReadyFromTerminal), errors.Is(err, errStageRaced), errors.Is(err, plan.ErrInvalidStage):
		writeReadyConflict(w, rt, taskID, err)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Operator just made a task dispatchable — wake the leader so it
	// picks up the work without waiting for the next pulse interval.
	// Hooked ONLY here, NOT in the MCP set_task_stage tool: that path is
	// leader-initiated and would self-wake recursively.
	if changed {
		d.autoWakeLeaderOnTaskReady(rt)
	}
	writeJSON(w, http.StatusOK, task)
}

// autoWakeLeaderOnTaskReady fires a pulse tick fire-and-forget after an
// operator successfully marks a task ready. Mirrors handlePingTeam's
// gating (channels-live → channel nudge; paused or busy → skip) so
// burst transitions don't pile up a queue of ticks.
//
// Safe to call with a nil-pulse team (no-op); the daemon's baseCtx is
// used for the Tick goroutine so it survives the HTTP request scope.
func (d *daemon) autoWakeLeaderOnTaskReady(rt *registeredTeam) {
	if rt == nil || rt.pulse == nil {
		return
	}
	if rt.channelsLive.Load() {
		publishPulseChannelNudge(rt.channelBus)
		if rt.auditSink != nil {
			_ = rt.auditSink.Write(audit.Event{
				Timestamp: time.Now().UTC(),
				AgentID:   "operator",
				Kind:      audit.KindPulseTick,
				Message:   "task ready: auto-wake routed as channel nudge",
				Meta:      map[string]any{"trigger": "task_ready", "route": "channel"},
			})
		}
		return
	}
	if rt.pulse.Paused() {
		return
	}
	if rt.pulse.Busy() {
		return
	}
	if rt.auditSink != nil {
		_ = rt.auditSink.Write(audit.Event{
			Timestamp: time.Now().UTC(),
			AgentID:   "operator",
			Kind:      audit.KindPulseTick,
			Message:   "task ready: auto-wake leader",
			Meta:      map[string]any{"trigger": "task_ready"},
		})
	}
	safeGo("pulse.task_ready:"+rt.team.ID, func() { _ = rt.pulse.Tick(d.baseCtx, "task_ready") })
}

// writeReadyConflict emits the JSON 409 body for /tasks/<id>/ready
// conflicts. The body shape is {"error": "<msg>", "current_stage":
// "<stage>"} so the SPA can refresh the row's stage on rejection. The
// current_stage field is best-effort: if the task has been deleted
// between the failed write and this re-read, it's omitted.
func writeReadyConflict(w http.ResponseWriter, rt *registeredTeam, taskID string, err error) {
	body := map[string]any{"error": err.Error()}
	if rt != nil && rt.plan != nil {
		if t, ok := rt.plan.Get(taskID); ok {
			body["current_stage"] = string(t.Stage)
		}
	}
	writeJSON(w, http.StatusConflict, body)
}

// splitTaskActionPath parses "<task_id>/<action>" out of the control
// subpath. Returns ok=false on shape mismatch.
func splitTaskActionPath(s string) (taskID, action string, ok bool) {
	slash := strings.IndexByte(s, '/')
	if slash < 0 {
		return "", "", false
	}
	taskID, action = s[:slash], s[slash+1:]
	if taskID == "" || action == "" || strings.Contains(action, "/") {
		return "", "", false
	}
	return taskID, action, true
}
