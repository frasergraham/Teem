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
// (stage transition + notes append), writes a decision_note audit
// event, and pushes a task_approval channel notification.
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

	// Channel: push a task_approval event so a connected leader
	// session sees the operator's action without polling. PushChannel
	// is a no-op when no session is subscribed.
	if rt.mcp != nil {
		rt.mcp.PushChannel(
			fmt.Sprintf("task %s: %s%s", taskID, string(d), commentPreview(comment)),
			map[string]string{
				"kind":     "task_approval",
				"task_id":  taskID,
				"decision": string(d),
				"comment":  comment,
			},
		)
	}

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

// commentPreview returns ": <first 80 chars>" for non-empty comments
// to attach to channel event content. Empty input yields "".
func commentPreview(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	const cap = 80
	if len(s) > cap {
		s = s[:cap] + "…"
	}
	return ": " + s
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

// handleTaskActionForm is the dashboard's form-POST counterpart of
// handleControlTaskAction. Reads form-encoded values, performs the
// same decision, and redirects back to the team page with a flash
// query param so the operator gets a visual confirmation.
//
// Unauth on purpose: same tailnet boundary the rest of the dashboard
// trusts. URL path: /teams/<id>/tasks/<task_id>/(approve|reject|comment).
func (d *daemon) handleTaskActionForm(w http.ResponseWriter, r *http.Request, rt *registeredTeam, taskID, action string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isSafeID(taskID) {
		http.Error(w, "bad task id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	req := taskActionRequest{
		Comment: r.PostForm.Get("comment"),
		Reason:  r.PostForm.Get("reason"),
	}
	dec, text, err := resolveActionInput(action, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, err = decideTask(rt, taskID, dec, text)
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
	flash := flashFor(dec)
	http.Redirect(w, r, fmt.Sprintf("/teams/%s?flash=%s", rt.team.ID, flash), http.StatusSeeOther)
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

func flashFor(d decision) string {
	switch d {
	case decisionApprove:
		return "task_approved"
	case decisionReject:
		return "task_rejected"
	case decisionComment:
		return "task_commented"
	}
	return "ok"
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

// resolveTaskActionRoute parses /tasks/<task_id>/(approve|reject|comment).
// Returns (taskID, action, true) on match; ("","",false) otherwise.
// Caller is expected to have already stripped the /teams/<id> prefix.
func resolveTaskActionRoute(suffix string) (taskID, action string, ok bool) {
	const prefix = "/tasks/"
	if !strings.HasPrefix(suffix, prefix) {
		return "", "", false
	}
	rest := suffix[len(prefix):]
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return "", "", false
	}
	taskID, action = rest[:slash], rest[slash+1:]
	if taskID == "" || strings.Contains(action, "/") {
		return "", "", false
	}
	switch action {
	case "approve", "reject", "comment":
		return taskID, action, true
	}
	return "", "", false
}
