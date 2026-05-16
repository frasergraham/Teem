package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/plan"
)

// resolveDecisionActionRoute parses /decisions/<task_id>/(reply|comment|unblock).
// Returns (taskID, action, true) on match; ("","",false) otherwise.
// Caller is expected to have already stripped the /teams/<id> prefix.
func resolveDecisionActionRoute(suffix string) (taskID, action string, ok bool) {
	const prefix = "/decisions/"
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
	case "reply", "comment", "unblock":
		return taskID, action, true
	}
	return "", "", false
}

// handleDecisionActionForm is the dashboard form-POST handler for the
// QUESTION and BLOCKER actions in the unified Decisions panel. These
// targets sit outside the awaiting_approval flow (whose endpoints check
// the task stage); the decisions endpoints accept the task in any stage
// and record an operator-side audit note that the leader can see and
// react to.
//
// Actions:
//   - reply / comment: append an operator decision_note (severity=info)
//     to the task, no stage change.
//   - unblock: transition the task from blocked → proposed and append
//     a decision_note so the leader notices and re-plans.
//
// Same tailnet-trust boundary as the rest of the dashboard form posts —
// no bearer check. Redirects back to the team page with a flash.
func (d *daemon) handleDecisionActionForm(w http.ResponseWriter, r *http.Request, rt *registeredTeam, taskID, action string) {
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
	comment := strings.TrimSpace(r.PostForm.Get("comment"))
	if rt.plan == nil {
		http.Error(w, "plan unavailable", http.StatusInternalServerError)
		return
	}
	cur, ok := rt.plan.Get(taskID)
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch action {
	case "unblock":
		if cur.Stage != plan.StageBlocked {
			http.Error(w, "task is not currently blocked", http.StatusConflict)
			return
		}
		if _, err := rt.plan.UpdateTask(taskID, plan.UpdateInput{
			Stage:  plan.StageProposed,
			Status: plan.StatusInProgress,
		}); err != nil {
			if errors.Is(err, plan.ErrInvalidStage) {
				http.Error(w, "cannot unblock: "+err.Error(), http.StatusConflict)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case "reply", "comment":
		// No stage mutation — this is just an operator-side note.
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}

	if rt.auditSink != nil {
		msg := decisionActionMessage(action, comment)
		_ = rt.auditSink.Write(audit.Event{
			Timestamp: time.Now().UTC(),
			AgentID:   "operator",
			Kind:      audit.KindDecisionNote,
			Message:   msg,
			Meta: map[string]any{
				"task_id":  taskID,
				"decision": action,
				"comment":  comment,
				"severity": "info",
			},
		})
	}
	// The hooked audit sink runs channelHook on every Write, so the
	// decision_note above already fans out to any connected leader
	// session — no explicit PushChannel is needed here.
	http.Redirect(w, r, fmt.Sprintf("/teams/%s?flash=task_commented", rt.team.ID), http.StatusSeeOther)
}

func decisionActionMessage(action, comment string) string {
	verb := "noted"
	switch action {
	case "reply":
		verb = "replied"
	case "comment":
		verb = "commented"
	case "unblock":
		verb = "unblocked task"
	}
	if comment == "" {
		return "operator " + verb
	}
	return "operator " + verb + ": " + comment
}
