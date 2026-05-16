package messaging

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/plan"
	"github.com/frasergraham/teem/internal/team"
)

// taskTitleLookup returns the live title of a task, or "" when the
// store can't resolve it. plan.Plan.Get matches the shape.
type taskTitleLookup func(taskID string) string

// MessageFormatter is the single place that lowers an audit.Event into a
// channel-agnostic Message. Notifiers don't format; they only ferry the
// rendered Message.
type MessageFormatter struct {
	TeamID           string
	DashboardBaseURL string
	TaskTitle        taskTitleLookup
	// LeaderStatus returns the leader's most recent update_leader_status
	// text (or "" when never set). Optional — nil falls back to the
	// generic "see dashboard" line on pulse_tick messages.
	LeaderStatus func() string
}

// FromPlan returns a TaskTitle lookup backed by a plan.Plan. Safe to
// pass a nil store — the lookup degrades to "".
func FromPlan(p *plan.Plan) taskTitleLookup {
	if p == nil {
		return func(string) string { return "" }
	}
	return func(id string) string {
		if t, ok := p.Get(id); ok {
			return t.Title
		}
		return ""
	}
}

// Format renders e into a Message. The bool is false when the event
// shouldn't produce a notification — invalid kind, missing required
// meta, or filter says "skip" (e.g. info-severity decisions).
func (f MessageFormatter) Format(e audit.Event) (Message, bool) {
	if !isMessagingKind(e) {
		return Message{}, false
	}
	taskID, _ := e.Meta["task_id"].(string)

	switch e.Kind {
	case audit.KindTaskStageChanged:
		to, _ := e.Meta["to"].(string)
		if to == "" {
			// formatChannelBody also accepts "stage" as the meta key
			// for the destination; honour that for forward-compat.
			to, _ = e.Meta["stage"].(string)
		}
		if to != "awaiting_approval" {
			return Message{}, false
		}
		return Message{
			Title:    fmt.Sprintf("[%s] Approval needed", f.TeamID),
			Summary:  f.summaryApproval(e.AgentID, taskID),
			Severity: SeverityAction,
			Link:     f.taskLink(taskID),
			TaskID:   taskID,
			AgentID:  e.AgentID,
			TeamID:   f.TeamID,
		}, true

	case audit.KindBlockerNote:
		return Message{
			Title:    fmt.Sprintf("[%s] Blocker on %s", f.TeamID, shortTaskID(taskID)),
			Summary:  f.summaryBlocker(e.AgentID, taskID, e.Message),
			Severity: SeverityWarning,
			Link:     f.taskLink(taskID),
			TaskID:   taskID,
			AgentID:  e.AgentID,
			TeamID:   f.TeamID,
		}, true

	case audit.KindDecisionNote:
		// Only the operator-must-see flavour: severity=question. Other
		// values (incl. unset = info) are journal entries.
		if sev, _ := e.Meta["severity"].(string); sev != "question" {
			return Message{}, false
		}
		return Message{
			Title:    fmt.Sprintf("[%s] Question from %s", f.TeamID, team.PersonaName(e.AgentID)),
			Summary:  f.summaryQuestion(taskID, e.Message),
			Severity: SeverityDecision,
			Link:     f.taskLink(taskID),
			TaskID:   taskID,
			AgentID:  e.AgentID,
			TeamID:   f.TeamID,
		}, true

	case audit.KindJobError:
		if e.AgentID != "leader" {
			return Message{}, false
		}
		return Message{
			Title:    fmt.Sprintf("[%s] Leader error", f.TeamID),
			Summary:  f.summaryLeaderError(e.JobID, e.Message),
			Severity: SeverityWarning,
			Link:     f.teamLink(),
			TaskID:   taskID,
			AgentID:  e.AgentID,
			TeamID:   f.TeamID,
		}, true

	case audit.KindPulseTick:
		return Message{
			Title:    fmt.Sprintf("[%s] Pulse tick", f.TeamID),
			Summary:  f.summaryPulseTick(e),
			Severity: SeverityInfo,
			// Pulse-tick link intentionally empty: the dashboard is
			// tailnet-only, and the Telegram message reaches the
			// operator on the public internet where the link wouldn't
			// resolve.
			Link:    "",
			TaskID:  taskID,
			AgentID: e.AgentID,
			TeamID:  f.TeamID,
		}, true
	}
	return Message{}, false
}

// isMessagingKind narrows the audit stream to the operator-must-see
// subset. Exported via the package-internal hook builder so tests can
// pin the filter directly.
func isMessagingKind(e audit.Event) bool {
	switch e.Kind {
	case audit.KindTaskStageChanged:
		to, _ := e.Meta["to"].(string)
		if to == "" {
			to, _ = e.Meta["stage"].(string)
		}
		return to == "awaiting_approval"
	case audit.KindBlockerNote:
		return true
	case audit.KindDecisionNote:
		sev, _ := e.Meta["severity"].(string)
		return sev == "question"
	case audit.KindJobError:
		return e.AgentID == "leader"
	case audit.KindPulseTick:
		return true
	}
	return false
}

// IsMessagingKind is the public test entry point — same body as the
// internal filter the hook uses.
func IsMessagingKind(e audit.Event) bool { return isMessagingKind(e) }

func (f MessageFormatter) summaryApproval(agentID, taskID string) string {
	who := team.PersonaName(agentID)
	if who == "" {
		who = agentID
	}
	if who == "" {
		who = "A worker"
	}
	title := ""
	if f.TaskTitle != nil {
		title = f.TaskTitle(taskID)
	}
	if title == "" {
		return fmt.Sprintf("%s finished %s and is waiting on approval.", who, shortTaskID(taskID))
	}
	return fmt.Sprintf("%s finished %s and wants you to look at %q.", who, shortTaskID(taskID), title)
}

func (f MessageFormatter) summaryBlocker(agentID, taskID, body string) string {
	who := team.PersonaName(agentID)
	if who == "" {
		who = agentID
	}
	tail := " Task moved to blocked."
	body = strings.TrimSpace(clipString(body, 200))
	if body == "" {
		return fmt.Sprintf("%s flagged a blocker on %s.%s", who, shortTaskID(taskID), tail)
	}
	return fmt.Sprintf("%s: %s%s", who, body, tail)
}

func (f MessageFormatter) summaryQuestion(taskID, body string) string {
	body = strings.TrimSpace(clipString(body, 200))
	if body == "" {
		return fmt.Sprintf("Leader has a question on %s.", shortTaskID(taskID))
	}
	if taskID != "" {
		return fmt.Sprintf("%s (see task %s)", body, shortTaskID(taskID))
	}
	return body
}

// summaryPulseTick prefers the leader's most recent update_leader_status
// text, then meta.summary, then a generic fallback. The leader's full
// pulse-tick claude output (in e.Message) is intentionally NOT used — it
// can be many KB and is for the audit log, not a phone notification.
func (f MessageFormatter) summaryPulseTick(e audit.Event) string {
	if f.LeaderStatus != nil {
		if s := strings.TrimSpace(f.LeaderStatus()); s != "" {
			return clipString(s, 200)
		}
	}
	if s, _ := e.Meta["summary"].(string); strings.TrimSpace(s) != "" {
		return clipString(strings.TrimSpace(s), 200)
	}
	return "Pulse tick — see dashboard for details."
}

func (f MessageFormatter) summaryLeaderError(jobID, body string) string {
	body = strings.TrimSpace(clipString(body, 200))
	if body == "" {
		body = "(no message)"
	}
	if jobID == "" {
		return fmt.Sprintf("Leader errored: %s", body)
	}
	return fmt.Sprintf("Leader's job %s errored: %s", jobID, body)
}

// taskLink builds a dashboard deep-link to the task card. The dashboard
// renders task cards with id="task-<task_id>" anchors today.
func (f MessageFormatter) taskLink(taskID string) string {
	base := strings.TrimRight(f.DashboardBaseURL, "/")
	if base == "" {
		return ""
	}
	team := url.PathEscape(f.TeamID)
	if taskID == "" {
		return fmt.Sprintf("%s/teams/%s/", base, team)
	}
	return fmt.Sprintf("%s/teams/%s/#task-%s", base, team, url.PathEscape(taskID))
}

func (f MessageFormatter) teamLink() string {
	base := strings.TrimRight(f.DashboardBaseURL, "/")
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/teams/%s/", base, url.PathEscape(f.TeamID))
}

// shortTaskID renders a task id in the short form the operator already
// sees on the dashboard (the prefix before the last hyphen-separated
// suffix). The plan today emits ids like t-3a2fbc01... — anything past
// 8 chars is just noise in prose.
func shortTaskID(id string) string {
	if id == "" {
		return "the task"
	}
	if strings.HasPrefix(id, "t-") && len(id) > 6 {
		return id[:6]
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func clipString(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
