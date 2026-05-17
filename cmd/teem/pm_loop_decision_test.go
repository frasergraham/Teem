package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/team"
)

// TestBuildTeamServices_StartsPMLoopWhenTrackerConfigured asserts the
// happy path that buildTeamServices runs through: tracker.type set +
// project_manager archetype present → pmLoopDecision returns run=true
// with a positive interval and no warn. This is the precondition that
// gates the goroutine spawn in buildTeamServices.
func TestBuildTeamServices_StartsPMLoopWhenTrackerConfigured(t *testing.T) {
	tt := &team.Team{
		ID:   "t-0000000000000001",
		Name: "alpha",
		Tracker: &team.TrackerConfig{
			Type:         "linear",
			TeamID:       "ENG",
			PollInterval: 30 * time.Minute,
		},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 1},
			{Role: team.PMArchetypeRole, Placement: "local", MaxConcurrent: 1},
		},
	}
	interval, run, warn := pmLoopDecision(tt)
	if !run {
		t.Fatalf("run = false, want true (tracker + PM archetype present)")
	}
	if warn != "" {
		t.Errorf("warn = %q, want empty", warn)
	}
	if interval != 30*time.Minute {
		t.Errorf("interval = %v, want 30m", interval)
	}
}

// TestBuildTeamServices_StartsPMLoopWithDefaultInterval covers the
// PollInterval=0 branch — buildTeamServices must still start the loop,
// defaulted to pmLoopDefaultInterval (1h). The yaml zero collapses
// "unset" and "explicit 0"; we treat both as "use the default".
func TestBuildTeamServices_StartsPMLoopWithDefaultInterval(t *testing.T) {
	tt := &team.Team{
		ID:   "t-0000000000000002",
		Name: "alpha",
		Tracker: &team.TrackerConfig{
			Type:   "linear",
			TeamID: "ENG",
			// PollInterval omitted on purpose.
		},
		Archetypes: []team.ArchetypeSpec{
			{Role: team.PMArchetypeRole, Placement: "local", MaxConcurrent: 1},
		},
	}
	interval, run, warn := pmLoopDecision(tt)
	if !run || warn != "" {
		t.Fatalf("run=%v warn=%q, want run=true warn=\"\"", run, warn)
	}
	if interval != pmLoopDefaultInterval {
		t.Errorf("interval = %v, want %v (default)", interval, pmLoopDefaultInterval)
	}
}

// TestBuildTeamServices_NoPMLoopWithoutTracker covers the no-tracker
// path: even with a project_manager archetype somehow present, no
// loop runs unless tracker.type is set. (handleRegister's PM-archetype
// synth gates on tracker too, so the archetype-without-tracker shape
// shouldn't appear in practice — but pmLoopDecision must still refuse
// to start the loop, with no warn since that's not a misconfiguration.)
func TestBuildTeamServices_NoPMLoopWithoutTracker(t *testing.T) {
	tt := &team.Team{
		ID:   "t-0000000000000003",
		Name: "alpha",
		// Tracker is nil.
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 1},
			{Role: team.PMArchetypeRole, Placement: "local", MaxConcurrent: 1},
		},
	}
	if _, run, warn := pmLoopDecision(tt); run || warn != "" {
		t.Errorf("Tracker=nil: run=%v warn=%q, want run=false warn=\"\"", run, warn)
	}

	tt.Tracker = &team.TrackerConfig{Type: ""}
	if _, run, warn := pmLoopDecision(tt); run || warn != "" {
		t.Errorf("Tracker.Type=\"\": run=%v warn=%q, want run=false warn=\"\"", run, warn)
	}
}

// TestBuildTeamServices_NoPMLoopWithoutArchetype covers the defensive
// branch: tracker is wired but the project_manager archetype is
// missing from the team. pmLoopDecision must refuse to start the loop
// AND emit a warn string the daemon will log to stderr — the operator
// needs to know the scheduled tick isn't running.
//
// In practice handleRegister/restoreTeams synth the PM archetype via
// MaybePMArchetype before calling buildTeamServices, so this branch is
// the safety net for a future regression where that synth no-ops.
func TestBuildTeamServices_NoPMLoopWithoutArchetype(t *testing.T) {
	tt := &team.Team{
		ID:   "t-0000000000000004",
		Name: "alpha",
		Tracker: &team.TrackerConfig{
			Type:         "linear",
			TeamID:       "ENG",
			PollInterval: time.Hour,
		},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 1},
		},
	}
	_, run, warn := pmLoopDecision(tt)
	if run {
		t.Errorf("run = true, want false (no PM archetype)")
	}
	if warn == "" {
		t.Fatalf("warn = \"\", want a non-empty operator-facing message")
	}
	if !strings.Contains(warn, "project_manager") {
		t.Errorf("warn does not mention the missing archetype: %q", warn)
	}
	if !strings.Contains(warn, "linear") {
		t.Errorf("warn does not surface tracker.type for diagnosability: %q", warn)
	}
}

// TestPMLoopDecision_NegativeIntervalDisables covers the
// operator-explicit-opt-out branch: a negative PollInterval disables
// the scheduled tick (on-demand spawn still works) and emits no warn —
// this is a supported configuration, not a misconfiguration.
func TestPMLoopDecision_NegativeIntervalDisables(t *testing.T) {
	tt := &team.Team{
		ID: "t-0000000000000005",
		Tracker: &team.TrackerConfig{
			Type:         "linear",
			TeamID:       "ENG",
			PollInterval: -1,
		},
		Archetypes: []team.ArchetypeSpec{
			{Role: team.PMArchetypeRole, Placement: "local", MaxConcurrent: 1},
		},
	}
	if _, run, warn := pmLoopDecision(tt); run || warn != "" {
		t.Errorf("negative PollInterval: run=%v warn=%q, want run=false warn=\"\"", run, warn)
	}
}

// TestPMLoopDecision_NilTeam guards the defensive nil branch — callers
// should not pass nil, but a panic here would corrupt the daemon's
// per-team build path.
func TestPMLoopDecision_NilTeam(t *testing.T) {
	if _, run, warn := pmLoopDecision(nil); run || warn != "" {
		t.Errorf("nil team: run=%v warn=%q, want run=false warn=\"\"", run, warn)
	}
}

// TestStopPMLoop_CancelsAndWaitsForGoroutine asserts the shutdown
// discipline buildTeamServices' callers rely on: stopPMLoop cancels
// the per-team PM context AND blocks until the goroutine's done
// channel closes, so the audit sink the loop writes to (and the
// spawner it pokes) cannot be torn down underneath an in-flight
// tick. The DELETE handler and daemon-shutdown teardown both depend
// on this.
func TestStopPMLoop_CancelsAndWaitsForGoroutine(t *testing.T) {
	fake := &fakePMSpawner{}
	sink := &captureSink{}
	cfg := PMLoopConfig{
		TeamName:     "test",
		Interval:     time.Hour, // never fires; cancellation is the only exit path
		Spawner:      fake,
		Audit:        sink,
		PollJobEvery: time.Millisecond,
	}
	pmCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var exited int32
	go func() {
		defer close(done)
		cfg.Loop(pmCtx)
		atomic.StoreInt32(&exited, 1)
	}()
	// Park briefly so the goroutine reaches the select on the ticker.
	time.Sleep(10 * time.Millisecond)

	rt := &registeredTeam{
		team:     &team.Team{ID: "t-0000000000000006", Name: "test"},
		pmCancel: cancel,
		pmDone:   done,
	}
	stopPMLoop(rt)

	// Goroutine must have returned before stopPMLoop returned.
	if atomic.LoadInt32(&exited) != 1 {
		t.Fatalf("goroutine did not exit before stopPMLoop returned")
	}
	if rt.pmCancel != nil {
		t.Errorf("rt.pmCancel not cleared after stopPMLoop")
	}
	if rt.pmDone != nil {
		t.Errorf("rt.pmDone not cleared after stopPMLoop")
	}

	// Idempotent: a second call must be a no-op (and must not panic).
	stopPMLoop(rt)
}

// TestStopPMLoop_NoOpWhenNotConfigured exercises the nil-pmCancel
// branch: teams without a tracker pass through the teardown paths
// just like tracker-configured ones; stopPMLoop must shrug.
func TestStopPMLoop_NoOpWhenNotConfigured(t *testing.T) {
	rt := &registeredTeam{team: &team.Team{ID: "t-0000000000000007", Name: "test"}}
	stopPMLoop(rt)  // must not panic
	stopPMLoop(nil) // must not panic on nil rt
}
