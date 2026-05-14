package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/frasergraham/teem/internal/audit"
)

// taskFlowView is the data passed to the task_flow template. Banner
// section pins current state (stage, time-in-stage, assignee);
// decisions/blockers panels surface non-trivial context; timeline
// rolls up every job linked to the task.
type taskFlowView struct {
	Team         string
	NowFormatted string
	Task         taskFlowBanner
	Decisions    []taskFlowNote
	Blockers     []taskFlowNote
	Timeline     []taskFlowJob
}

type taskFlowBanner struct {
	ID             string
	Title          string
	Status         string
	Stage          string
	StageEnteredIn string // duration since stage entry, "—" when unknown
	StageEnteredAt string // local-time clock format for tooltip clarity
	AssignedTo     string
	Notes          string
	UpdatedAgo     string
	Evidence       []string // job ids
}

type taskFlowNote struct {
	Time    string
	AgentID string
	Text    string
}

type taskFlowJob struct {
	JobID        string
	AgentID      string
	StartedAt    time.Time
	StartedAgo   string
	StartedShort string
	Duration     string
	Status       string
	Prompt       string
	Summary      string
	JobURL       string
}

// renderTaskFlow writes the per-task flow page: banner + decisions +
// blockers + chronological job timeline. Joins plan (for stage),
// audit (for decision/blocker notes), and materialized jobs (for
// timeline entries).
func (d *daemon) renderTaskFlow(w http.ResponseWriter, _ *http.Request, rt *registeredTeam, taskID string) {
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
	view := taskFlowView{
		Team:         rt.team.Name,
		NowFormatted: time.Now().Local().Format("Mon Jan 2 15:04:05"),
		Task: taskFlowBanner{
			ID:         task.ID,
			Title:      task.Title,
			Status:     string(task.Status),
			Stage:      string(task.Stage),
			AssignedTo: task.AssignedTo,
			Notes:      task.Notes,
			UpdatedAgo: agoShort(task.UpdatedAt),
			Evidence:   append([]string(nil), task.Evidence...),
		},
	}
	if !task.StageEnteredAt.IsZero() {
		view.Task.StageEnteredIn = agoShort(task.StageEnteredAt)
		view.Task.StageEnteredAt = task.StageEnteredAt.Local().Format("Mon Jan 2 15:04:05")
	} else {
		view.Task.StageEnteredIn = "—"
	}

	// Audit pass: decisions + blockers tagged with this task_id; pull
	// a generous window so older records aren't lost.
	if rt.auditSink != nil {
		events, err := rt.auditSink.Query("", time.Time{}, 0)
		if err == nil {
			for _, e := range events {
				if id, _ := e.Meta["task_id"].(string); id != task.ID {
					continue
				}
				switch e.Kind {
				case audit.KindDecisionNote:
					view.Decisions = append(view.Decisions, taskFlowNote{
						Time:    timeShort(e.Timestamp),
						AgentID: e.AgentID,
						Text:    e.Message,
					})
				case audit.KindBlockerNote:
					view.Blockers = append(view.Blockers, taskFlowNote{
						Time:    timeShort(e.Timestamp),
						AgentID: e.AgentID,
						Text:    e.Message,
					})
				}
			}
		}

		// Timeline: materialize the linked jobs from evidence ids.
		if len(task.Evidence) > 0 {
			allJobs := audit.MaterializeJobs(events)
			byID := map[string]audit.MaterializedJob{}
			for _, j := range allJobs {
				byID[j.JobID] = j
			}
			for _, jid := range task.Evidence {
				j, ok := byID[jid]
				if !ok {
					// Audit pruned but task references it — render a
					// stub so the operator can still see it exists.
					view.Timeline = append(view.Timeline, taskFlowJob{
						JobID:  jid,
						Status: "unknown",
						JobURL: fmt.Sprintf("/teams/%s/jobs/%s", rt.team.ID, jid),
					})
					continue
				}
				view.Timeline = append(view.Timeline, taskFlowJob{
					JobID:        j.JobID,
					AgentID:      j.AgentID,
					StartedAt:    j.StartedAt,
					StartedAgo:   agoShort(j.StartedAt),
					StartedShort: timeShort(j.StartedAt),
					Duration:     durShort(j.Duration()),
					Status:       j.Status,
					Prompt:       j.Prompt,
					Summary:      j.Summary,
					JobURL:       fmt.Sprintf("/teams/%s/jobs/%s", rt.team.ID, j.JobID),
				})
			}
			// Sort by absolute timestamp so a timeline spanning midnight
			// stays chronologically ordered (HH:MM:SS strings can't).
			sort.SliceStable(view.Timeline, func(i, j int) bool {
				return view.Timeline[i].StartedAt.Before(view.Timeline[j].StartedAt)
			})
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := uiTemplates.ExecuteTemplate(w, "task_flow", view); err != nil {
		fmt.Printf("[teemd] task_flow render: %v\n", err)
	}
}

// resolveTaskFlowRoute parses /teams/<team>/tasks/<taskID>. Returns
// the task_id when the suffix matches; "" means not this route.
func resolveTaskFlowRoute(suffix string) (string, bool) {
	const prefix = "/tasks/"
	if !strings.HasPrefix(suffix, prefix) {
		return "", false
	}
	id := suffix[len(prefix):]
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}
