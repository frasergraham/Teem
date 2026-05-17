package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/team"
)

// pmLoopDefaultInterval is the scheduled-tick cadence used when a
// tracker-configured team leaves TrackerConfig.PollInterval unset.
// 1h matches the manual consultation cadence the leader prompt
// suggests; tracker-side movement rarely needs faster pickup.
const pmLoopDefaultInterval = time.Hour

// pmJobWaitTimeout caps how long a single PM tick will wait for its
// assigned consultation job to finish before the loop stops the agent
// and moves on. Generous enough to absorb a slow tracker round-trip
// without leaving zombie PMs in place if the worker hangs.
const pmJobWaitTimeout = 15 * time.Minute

// pmJobPollInterval is the cadence the loop polls JobStatus at while
// waiting for the consultation job to land. Tests override via
// PMLoopConfig.PollJobEvery.
const pmJobPollInterval = 500 * time.Millisecond

// pmConsultationBrief is the standing prompt assigned to every
// scheduled PM tick. Kept short and self-contained — the PM worker
// reads it without any conversation context. The leader's prompt
// already documents that tracker-side activity may surface as
// add_task entries on this cadence.
const pmConsultationBrief = `Scheduled project-manager consultation tick.

Your job on this tick:

1. Read recent team activity. Pull the last few hours of audit events with
   query_audit (kind filters: job_complete, task_stage_changed,
   decision_note are the most useful) and the current plan with list_tasks.

2. Push tracker updates for completed work. For every Teem task that moved
   to stage=verified since your last tick, post a short comment to the
   corresponding tracker issue summarising the outcome. Do not invent
   tracker IDs — only update issues already linked from a Teem task.

3. Surface tracker-side work into the plan. List open tracker issues
   assigned to the team that do NOT yet have a matching Teem task and
   create one each via add_task. Title = the tracker issue title; notes =
   a one-line link back ("tracker: <issue-id>") plus any acceptance
   criteria worth carrying over. Leave new tasks in stage=todo for the
   leader to triage — you do not assign jobs or move stages.

4. Report back when done. One paragraph: how many tracker comments you
   posted, how many add_task entries you created, and anything the leader
   should know about tracker-side priority shifts.

You are a consultant on this tick, not an orchestrator: never call
spawn_agent, assign_job, set_task_stage, or any tool that would change
the team's workflow state. In particular, never move a task in stage=ready
back to specced or proposed — that is the operator's pre-flight signal.
The leader owns those decisions.`

// pmLoopDecision is the pure policy buildTeamServices consults to
// decide whether to start the per-team PM goroutine. It returns the
// effective interval, a boolean run flag, and an optional warn string
// for the caller to log on stderr.
//
// Rules:
//   - Tracker == nil or Type == "" → no loop, no warn (team is not
//     tracker-configured; the leader prompt's PM mention is moot).
//   - Tracker set but no project_manager archetype on the team → no
//     loop, warn message returned so the operator notices the gap.
//     (handleRegister/restoreTeams synth the PM archetype before
//     buildTeamServices; this branch is the defensive fallback if
//     the synth ever silently fails to land.)
//   - PollInterval == 0 → default to pmLoopDefaultInterval (1h). The
//     YAML zero value collapses "unset" and "explicit 0" — we treat
//     both as "use the default".
//   - PollInterval < 0 → no loop, no warn (operator explicitly
//     disabled scheduled ticks; on-demand spawn still works).
//   - Otherwise → run = true with the chosen interval.
func pmLoopDecision(t *team.Team) (interval time.Duration, run bool, warn string) {
	if t == nil || t.Tracker == nil || t.Tracker.Type == "" {
		return 0, false, ""
	}
	hasPM := false
	for _, a := range t.Archetypes {
		if a.Role == team.PMArchetypeRole {
			hasPM = true
			break
		}
	}
	if !hasPM {
		return 0, false, fmt.Sprintf("tracker.type=%q but no %s archetype declared; PM loop not started", t.Tracker.Type, team.PMArchetypeRole)
	}
	interval = t.Tracker.PollInterval
	if interval == 0 {
		interval = pmLoopDefaultInterval
	}
	if interval <= 0 {
		return 0, false, ""
	}
	return interval, true, ""
}

// pmSpawner is the slice of the agent.Spawner surface the PM loop
// drives. Pulled out as an interface so the unit test can inject a
// recording fake without standing up a real spawner / provisioner /
// bus stack. The production binding is *agent.Spawner, which already
// satisfies it.
type pmSpawner interface {
	Spawn(ctx context.Context, role, name string) (string, error)
	AssignJob(ctx context.Context, agentID, prompt, contextNote string) (string, error)
	JobStatus(jobID string) (string, string, bool)
	StopAgent(ctx context.Context, agentID string) error
}

// auditWriter narrows audit.Sink to the single method the loop needs.
// Sink itself is fine in production; tests pass a record-and-replay
// fake.
type auditWriter interface {
	Write(e audit.Event) error
}

// PMLoopConfig wires one team's scheduled project-manager tick. It is
// constructed by the daemon in buildTeamServices/restoreTeams when the
// team is tracker-configured (Team.Tracker.Type != ""), and run on a
// goroutine scoped to the daemon's baseCtx.
type PMLoopConfig struct {
	// TeamName is included in stderr log lines so the operator can tell
	// which team is ticking. Not used for routing.
	TeamName string
	// Interval is the cadence. <= 0 means "the loop never runs" —
	// callers check this before spawning the goroutine.
	Interval time.Duration
	// Spawner is the per-team agent.Spawner. Required.
	Spawner pmSpawner
	// Audit is the per-team audit sink. Required.
	Audit auditWriter
	// Brief overrides the standing consultation prompt. Empty means
	// pmConsultationBrief. The override exists for tests that don't
	// care about the prompt text.
	Brief string
	// PollJobEvery overrides pmJobPollInterval. Tests use a tiny value
	// so they don't sleep 500ms per status check.
	PollJobEvery time.Duration
	// JobWaitTimeout overrides pmJobWaitTimeout. Tests set this low so
	// a stuck JobStatus doesn't hold the test goroutine for 15m.
	JobWaitTimeout time.Duration
	// Now lets tests stub the clock for audit Timestamps. Defaults to
	// time.Now.UTC.
	Now func() time.Time
}

// Loop is the long-running goroutine body. Returns when ctx is done.
// First tick fires after one Interval (no warm-up like prune does;
// scheduled PM consultations are not interesting enough on boot to
// justify the extra tick).
func (c PMLoopConfig) Loop(ctx context.Context) {
	if c.Interval <= 0 {
		return
	}
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.tick(ctx)
		}
	}
}

// tick runs one consultation pass: try to spawn a project_manager
// worker; on "at capacity" log skipped_overlap and bail; otherwise
// assign the standing brief, wait for completion, and retire the
// worker. Every path emits a pm_tick audit event so the operator can
// see the cadence in `teem audit`.
func (c PMLoopConfig) tick(ctx context.Context) {
	agentID, err := c.Spawner.Spawn(ctx, team.PMArchetypeRole, "")
	if err != nil {
		outcome := audit.PMOutcomeError
		if isAtCapacityErr(err) {
			outcome = audit.PMOutcomeSkippedOverlap
		}
		c.writeAudit(audit.Event{
			Kind:    audit.KindPMTick,
			Message: err.Error(),
			Meta:    map[string]any{"outcome": outcome},
		})
		return
	}

	brief := c.Brief
	if brief == "" {
		brief = pmConsultationBrief
	}
	jobID, err := c.Spawner.AssignJob(ctx, agentID, brief, "")
	if err != nil {
		c.writeAudit(audit.Event{
			AgentID: agentID,
			Kind:    audit.KindPMTick,
			Message: "assign_job: " + err.Error(),
			Meta:    map[string]any{"outcome": audit.PMOutcomeError},
		})
		// Retire the worker even on assign failure — leaving it idle
		// would leak the capacity slot.
		_ = c.Spawner.StopAgent(ctx, agentID)
		return
	}

	c.waitForJob(ctx, jobID)
	// Best-effort retire. If the daemon is shutting down, ctx is already Done
	// and the spawner's shutdown path will tear the agent down via its own loop.
	if err := c.Spawner.StopAgent(ctx, agentID); err != nil {
		fmt.Fprintf(os.Stderr, "[pm_loop] %s: stop %s: %v\n", c.TeamName, agentID, err)
	}
	c.writeAudit(audit.Event{
		AgentID: agentID,
		JobID:   jobID,
		Kind:    audit.KindPMTick,
		Meta:    map[string]any{"outcome": audit.PMOutcomeSpawned},
	})
}

// waitForJob polls JobStatus until the job is terminal (status="done"
// or "error"), the context is cancelled, or JobWaitTimeout fires.
// Polling rather than subscribing keeps the loop self-contained — the
// agent.Spawner's result subscriber already owns the bus side.
func (c PMLoopConfig) waitForJob(ctx context.Context, jobID string) {
	timeout := c.JobWaitTimeout
	if timeout <= 0 {
		timeout = pmJobWaitTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	poll := c.PollJobEvery
	if poll <= 0 {
		poll = pmJobPollInterval
	}
	// Fast path: many tests (and the no-op happy path) flip the fake
	// to "done" before AssignJob returns. Check once before parking on
	// the ticker so we don't waste a polling interval.
	if status, _, ok := c.Spawner.JobStatus(jobID); ok && isTerminalStatus(status) {
		return
	}
	tk := time.NewTicker(poll)
	defer tk.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return
		case <-tk.C:
			status, _, ok := c.Spawner.JobStatus(jobID)
			if !ok {
				continue
			}
			if isTerminalStatus(status) {
				return
			}
		}
	}
}

func isTerminalStatus(s string) bool { return s == "done" || s == "error" }

// isAtCapacityErr matches the sentinel string the spawner emits when
// an archetype's MaxConcurrent cap is full. The spawner returns plain
// fmt.Errorf-wrapped errors there (no exported var to compare
// against), so substring is the contract.
func isAtCapacityErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "at capacity")
}

func (c PMLoopConfig) writeAudit(e audit.Event) {
	if c.Audit == nil {
		return
	}
	if e.Timestamp.IsZero() {
		if c.Now != nil {
			e.Timestamp = c.Now()
		} else {
			e.Timestamp = time.Now().UTC()
		}
	}
	if err := c.Audit.Write(e); err != nil {
		fmt.Fprintf(os.Stderr, "[pm_loop] %s: audit write: %v\n", c.TeamName, err)
	}
}
