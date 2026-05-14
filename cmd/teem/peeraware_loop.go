package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/frasergraham/teem/internal/archmem"
	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/peeraware"
	"github.com/frasergraham/teem/internal/plan"
)

// peerAwareBlockName is the archmem block label that the cross-project
// digest is written under. AppendBlock replaces this block in-place
// each tick so the leader memory file stays bounded.
const peerAwareBlockName = "peer-projects"

// peerAwareDefaultInterval is the cross-project digest cadence.
// XP1 ships with this enabled out of the box; operators can disable it
// with TEEM_PEERAWARE_INTERVAL=0.
const peerAwareDefaultInterval = time.Hour

// peerAwareConfig returns the digest interval from the env, defaulting
// to peerAwareDefaultInterval. A value of 0 / "off" / "never" disables
// the goroutine entirely.
func peerAwareConfig() time.Duration {
	return parsePeerAwareInterval(os.Getenv("TEEM_PEERAWARE_INTERVAL"), func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format, args...)
	})
}

// parsePeerAwareInterval is the env-agnostic core of peerAwareConfig.
// logf reports a parse failure on the bad-value path; tests inject a
// capturing logger here instead of poking stderr.
func parsePeerAwareInterval(raw string, logf func(format string, args ...any)) time.Duration {
	v := strings.TrimSpace(strings.ToLower(raw))
	switch v {
	case "":
		return peerAwareDefaultInterval
	case "0", "off", "never", "disabled":
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		if logf != nil {
			logf("[peeraware] bad TEEM_PEERAWARE_INTERVAL %q — using default %s\n", v, peerAwareDefaultInterval)
		}
		return peerAwareDefaultInterval
	}
	return d
}

// runPeerAware ticks on the configured interval and, for each registered
// team, writes a "peer projects" digest into that team's leader memory.
// The first pass runs 60s after startup so a developer iterating on the
// feature can observe behaviour without waiting an hour.
func (d *daemon) runPeerAware(interval time.Duration) {
	if interval <= 0 {
		return
	}
	select {
	case <-d.baseCtx.Done():
		return
	case <-time.After(60 * time.Second):
	}
	d.peerAwareSweep(interval)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-d.baseCtx.Done():
			return
		case <-t.C:
			d.peerAwareSweep(interval)
		}
	}
}

// peerAwareSweep runs one digest pass across every registered team. The
// "past window" matches the cadence — at a 1h cadence each tick reports
// what's moved in the previous hour. Per-team snapshot collection is
// wrapped in a recover so one bad team can't kill the loop or leak into
// another's digest.
func (d *daemon) peerAwareSweep(window time.Duration) {
	d.mu.Lock()
	teams := make([]*registeredTeam, 0, len(d.teams))
	for _, rt := range d.teams {
		teams = append(teams, rt)
	}
	d.mu.Unlock()
	if len(teams) < 2 {
		// One (or zero) teams registered — nothing to be aware of.
		return
	}
	now := time.Now().UTC()
	cutoff := now.Add(-window)

	snaps := make([]peeraware.Snapshot, 0, len(teams))
	for _, rt := range teams {
		snap, ok := collectSnapshotSafe(rt, cutoff)
		if !ok {
			continue
		}
		snaps = append(snaps, snap)
	}

	for _, rt := range teams {
		writeDigestSafe(rt, snaps, now)
	}
}

func collectSnapshotSafe(rt *registeredTeam, cutoff time.Time) (snap peeraware.Snapshot, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "[peeraware] %s: snapshot panic: %v\n", rt.team.Name, r)
			snap = peeraware.Snapshot{}
			ok = false
		}
	}()
	return collectSnapshot(rt, cutoff), true
}

func writeDigestSafe(rt *registeredTeam, snaps []peeraware.Snapshot, now time.Time) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "[peeraware] %s: write panic: %v\n", rt.team.Name, r)
		}
	}()
	body := peeraware.Digest(rt.team.Name, snaps, now)
	if strings.TrimSpace(body) == "" {
		return
	}
	if rt.archMem == nil {
		return
	}
	if err := rt.archMem.AppendBlock(archmem.LeaderRole, peerAwareBlockName, body); err != nil {
		fmt.Fprintf(os.Stderr, "[peeraware] %s: write leader memory block: %v\n", rt.team.Name, err)
	}
}

// collectSnapshot reads the bits of in-memory team state the digester
// needs. Fast paths only: registry list, plan list, leader-status get,
// and an audit query bounded to past-window events.
func collectSnapshot(rt *registeredTeam, cutoff time.Time) peeraware.Snapshot {
	snap := peeraware.Snapshot{Team: rt.team.Name}

	if rt.leaderStatus != nil {
		if e, ok := rt.leaderStatus.Get("leader"); ok {
			// Only surface leader_status if it's fresh within the same
			// window we use for task activity. A stale status would
			// otherwise trip a fresh peer block on every tick long
			// after the leader has gone idle.
			if !e.UpdatedAt.IsZero() && !e.UpdatedAt.Before(cutoff) {
				snap.LeaderStatus = e.Text
				snap.LeaderUpdated = e.UpdatedAt
			}
		}
	}

	if rt.plan != nil {
		for _, t := range rt.plan.List(plan.Filter{}) {
			stage := string(t.Stage)
			switch t.Stage {
			case plan.StagePlanning, plan.StageCoding, plan.StageReviewing, plan.StageIntegrating, plan.StageBlocked:
				snap.OpenTasks = append(snap.OpenTasks, peeraware.TaskBrief{
					ID:    t.ID,
					Title: t.Title,
					Stage: stage,
				})
			}
			if !t.StageEnteredAt.Before(cutoff) {
				switch t.Stage {
				case plan.StageVerified:
					snap.JustVerified = append(snap.JustVerified, peeraware.TaskBrief{
						ID:    t.ID,
						Title: t.Title,
						Stage: stage,
					})
				case plan.StageBlocked:
					snap.JustBlocked = append(snap.JustBlocked, peeraware.TaskBrief{
						ID:    t.ID,
						Title: t.Title,
						Stage: stage,
					})
				}
			}
		}
	}

	if rt.registry != nil {
		for _, e := range rt.registry.List() {
			if e.State == "stopped" || e.State == "error" {
				continue
			}
			snap.ActiveAgents = append(snap.ActiveAgents, peeraware.AgentBrief{
				ID:    e.ID,
				Role:  e.Role,
				State: string(e.State),
			})
		}
	}

	if rt.auditSink != nil {
		events, err := rt.auditSink.Query("", cutoff, 0)
		if err == nil {
			for _, e := range events {
				switch e.Kind {
				case audit.KindDecisionNote:
					tid, _ := e.Meta["task_id"].(string)
					snap.Decisions = append(snap.Decisions, peeraware.NoteBrief{
						TaskID: tid,
						Text:   e.Message,
						When:   e.Timestamp,
					})
				case audit.KindBlockerNote:
					tid, _ := e.Meta["task_id"].(string)
					snap.Blockers = append(snap.Blockers, peeraware.NoteBrief{
						TaskID: tid,
						Text:   e.Message,
						When:   e.Timestamp,
					})
				}
			}
		}
	}

	return snap
}
