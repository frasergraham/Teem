package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/pruner"
)

// pruneSweepDefaultInterval is the periodic-sweep cadence baked in
// when TEEM_PRUNE_INTERVAL is unset. 12h matches the dead-branch
// accumulation rate observed before this feature shipped — slow
// enough that a busy day still shows useful state, fast enough that
// the operator never sees more than a couple dozen retired branches
// at once.
const pruneSweepDefaultInterval = 12 * time.Hour

// pruneSweepConfig returns the configured interval, defaulting to 12h.
// 0 / "off" / "never" / "disabled" disables the loop entirely.
func pruneSweepConfig() time.Duration {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("TEEM_PRUNE_INTERVAL")))
	switch v {
	case "":
		return pruneSweepDefaultInterval
	case "0", "off", "never", "disabled":
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "[teemd] bad TEEM_PRUNE_INTERVAL %q — using default %s\n", v, pruneSweepDefaultInterval)
		return pruneSweepDefaultInterval
	}
	return d
}

// runPruneSweep is the daemon's periodic branch-cleanup goroutine.
// First pass fires 60s after startup (so a developer poking at this
// can observe behaviour without waiting half a day), then on the
// configured interval.
func (d *daemon) runPruneSweep(interval time.Duration) {
	if interval <= 0 {
		return
	}
	select {
	case <-d.baseCtx.Done():
		return
	case <-time.After(60 * time.Second):
	}
	d.pruneSweep()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-d.baseCtx.Done():
			return
		case <-t.C:
			d.pruneSweep()
		}
	}
}

// pruneSweep runs `prune-branches --yes` semantics against each
// registered team's repo. Teams without a repoRoot are skipped
// silently (Fargate-only / repo-less teams have no branches to prune).
func (d *daemon) pruneSweep() {
	d.mu.Lock()
	teams := make([]*registeredTeam, 0, len(d.teams))
	for _, rt := range d.teams {
		teams = append(teams, rt)
	}
	d.mu.Unlock()

	for _, rt := range teams {
		if rt.repoRoot == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(d.baseCtx, 60*time.Second)
		d.pruneOneTeam(ctx, rt)
		cancel()
	}
}

func (d *daemon) pruneOneTeam(ctx context.Context, rt *registeredTeam) {
	live := liveAgentIDs(rt.registry)
	rosterEntries := rt.spawner.RosterSnapshot("")
	rosterView := make([]pruner.RosterView, 0, len(rosterEntries))
	for _, e := range rosterEntries {
		rosterView = append(rosterView, pruner.RosterView{
			AgentID: e.ID, InUse: e.InUse, LastUsedAt: e.LastUsedAt,
		})
		// Belt-and-suspenders: an InUse roster entry implies live
		// even if the in-memory registry has drifted (e.g. a worker
		// the daemon hasn't fully reconciled yet).
		if e.InUse {
			live[e.ID] = true
		}
	}

	branches, err := pruner.LoadCandidates(ctx, rt.repoRoot, "main")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[teem] prune %s: load candidates: %v\n", rt.team.Name, err)
		return
	}
	cls := pruner.Classify(pruner.Inputs{
		Branches: branches,
		Roster:   rosterView,
		Live:     live,
		Now:      time.Now(),
	})

	// Race window: the snapshot above (live + roster + branches) takes
	// ~1s; a worker can be spawned between then and the branch -d
	// below. LiveCheck closes that window — Apply re-queries the
	// registry immediately before each branch deletion, and InUse
	// roster entries also count as live (handles the not-yet-fully-
	// reconciled case).
	liveCheck := func(agentID string) bool {
		if rt.registry == nil {
			return false
		}
		if e, ok := rt.registry.Get(agentID); ok && e.State != mcpsrv.StateStopped {
			return true
		}
		for _, e := range rt.spawner.RosterSnapshot("") {
			if e.ID == agentID && e.InUse {
				return true
			}
		}
		return false
	}

	res := pruner.Apply(ctx, cls, pruner.SweepOpts{
		RepoRoot:     rt.repoRoot,
		WorktreeBase: defaultWorktreeBase(rt.team.Name),
		// Periodic sweep never forces — it only ever deletes merged
		// branches automatically. Retired / orphan / unmerged sit
		// until the operator runs `teem prune-branches --yes --force`.
		Force:     false,
		LiveCheck: liveCheck,
		Logf: func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, "[teem] "+format+"\n", a...)
		},
	})
	if len(res.Deleted) > 0 {
		fmt.Fprintf(os.Stderr, "[teem] prune %s: deleted %d branch(es)\n", rt.team.Name, len(res.Deleted))
	}
	for name, err := range res.Errors {
		fmt.Fprintf(os.Stderr, "[teem] prune %s: %s: %v\n", rt.team.Name, name, err)
	}
}

// liveAgentIDs collects agent ids the registry considers actively
// running (not yet in StateStopped). The pruner uses this as the
// "do not touch" set.
func liveAgentIDs(reg *mcpsrv.Registry) map[string]bool {
	live := map[string]bool{}
	if reg == nil {
		return live
	}
	for _, e := range reg.List() {
		if e.State != mcpsrv.StateStopped {
			live[e.ID] = true
		}
	}
	return live
}
