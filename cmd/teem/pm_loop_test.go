package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
)

// fakePMSpawner records what the loop calls and returns scripted
// results. Spawn always returns "pm-ada" unless SpawnErr is set;
// AssignJob always returns "job-1" unless AssignErr is set; JobStatus
// returns "done" on the first call unless overridden.
type fakePMSpawner struct {
	mu sync.Mutex

	spawnErr   error
	assignErr  error
	statusSeq  []string // status returned per call; empty falls back to "done"
	statusIdx  int
	stopErr    error
	stopCalled bool

	spawnCalls  int32
	assignCalls int32
	statusCalls int32
}

func (f *fakePMSpawner) Spawn(_ context.Context, _, _ string) (string, error) {
	atomic.AddInt32(&f.spawnCalls, 1)
	f.mu.Lock()
	err := f.spawnErr
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	return "pm-ada", nil
}

func (f *fakePMSpawner) AssignJob(_ context.Context, _, _, _ string) (string, error) {
	atomic.AddInt32(&f.assignCalls, 1)
	f.mu.Lock()
	err := f.assignErr
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	return "job-1", nil
}

func (f *fakePMSpawner) JobStatus(_ string) (string, string, bool) {
	atomic.AddInt32(&f.statusCalls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusIdx < len(f.statusSeq) {
		s := f.statusSeq[f.statusIdx]
		f.statusIdx++
		return s, "", true
	}
	return "done", "", true
}

func (f *fakePMSpawner) StopAgent(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalled = true
	return f.stopErr
}

// captureSink is an auditWriter that holds events in memory.
type captureSink struct {
	mu     sync.Mutex
	events []audit.Event
}

func (c *captureSink) Write(e audit.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return nil
}

func (c *captureSink) snapshot() []audit.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]audit.Event, len(c.events))
	copy(out, c.events)
	return out
}

func TestPMLoop_FiresSpawnOnCadence(t *testing.T) {
	fake := &fakePMSpawner{}
	sink := &captureSink{}
	cfg := PMLoopConfig{
		TeamName:       "test",
		Interval:       10 * time.Millisecond,
		Spawner:        fake,
		Audit:          sink,
		Brief:          "test brief",
		PollJobEvery:   time.Millisecond,
		JobWaitTimeout: time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		cfg.Loop(ctx)
		close(done)
	}()

	// Wait for at least 3 ticks worth of spawns.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&fake.spawnCalls) >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if got := atomic.LoadInt32(&fake.spawnCalls); got < 3 {
		t.Fatalf("spawn called %d times, want >= 3", got)
	}
	if got := atomic.LoadInt32(&fake.assignCalls); got < 3 {
		t.Errorf("assign called %d times, want >= 3", got)
	}
	if !fake.stopCalled {
		t.Errorf("StopAgent never called")
	}
	// Each completed tick emits one pm_tick with outcome=spawned.
	var spawned int
	for _, e := range sink.snapshot() {
		if e.Kind != audit.KindPMTick {
			t.Errorf("unexpected audit kind %q", e.Kind)
			continue
		}
		if e.Meta["outcome"] == "spawned" {
			spawned++
		}
	}
	if spawned < 3 {
		t.Errorf("got %d spawned audit events, want >= 3", spawned)
	}
}

func TestPMLoop_ContextCancelExits(t *testing.T) {
	fake := &fakePMSpawner{}
	sink := &captureSink{}
	cfg := PMLoopConfig{
		TeamName:     "test",
		Interval:     time.Hour, // long enough that the ticker never fires
		Spawner:      fake,
		Audit:        sink,
		PollJobEvery: time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		cfg.Loop(ctx)
		close(done)
	}()

	// Give the goroutine a moment to park on the ticker.
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("loop did not exit within 1s of ctx cancel")
	}
	if got := atomic.LoadInt32(&fake.spawnCalls); got != 0 {
		t.Errorf("spawn called %d times before cancel, want 0", got)
	}
}

func TestPMLoop_AtCapacityLogsSkippedOverlap(t *testing.T) {
	fake := &fakePMSpawner{
		spawnErr: errors.New(`archetype "project_manager" is at capacity (1/1 running)`),
	}
	sink := &captureSink{}
	cfg := PMLoopConfig{
		TeamName:     "test",
		Interval:     10 * time.Millisecond,
		Spawner:      fake,
		Audit:        sink,
		PollJobEvery: time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		cfg.Loop(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&fake.spawnCalls) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if got := atomic.LoadInt32(&fake.assignCalls); got != 0 {
		t.Errorf("AssignJob called %d times after at-capacity Spawn, want 0", got)
	}

	events := sink.snapshot()
	if len(events) == 0 {
		t.Fatalf("no audit events written")
	}
	var skipped int
	for _, e := range events {
		if e.Kind != audit.KindPMTick {
			t.Errorf("unexpected audit kind %q", e.Kind)
			continue
		}
		if e.Meta["outcome"] == "skipped_overlap" {
			skipped++
		}
	}
	if skipped < 2 {
		t.Errorf("got %d skipped_overlap events, want >= 2", skipped)
	}
}

func TestPMLoop_AssignErrorRetiresAgent(t *testing.T) {
	fake := &fakePMSpawner{assignErr: errors.New("bus closed")}
	sink := &captureSink{}
	cfg := PMLoopConfig{
		TeamName:     "test",
		Interval:     10 * time.Millisecond,
		Spawner:      fake,
		Audit:        sink,
		PollJobEvery: time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		cfg.Loop(ctx)
		close(done)
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&fake.spawnCalls) >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done

	if !fake.stopCalled {
		t.Errorf("StopAgent not called after AssignJob error — capacity slot would leak")
	}
	var sawError bool
	for _, e := range sink.snapshot() {
		if e.Kind == audit.KindPMTick && e.Meta["outcome"] == "error" {
			sawError = true
		}
	}
	if !sawError {
		t.Errorf("no pm_tick outcome=error event after assign failure")
	}
}

// TestPMConsultationBrief_ProtectsReadyStage asserts the standing
// per-tick PM prompt forbids reverting stage=ready, so the PM worker
// can't silently undo the operator's pre-flight signal (t-b252d388).
func TestPMConsultationBrief_ProtectsReadyStage(t *testing.T) {
	if !strings.Contains(pmConsultationBrief, "stage=ready") {
		t.Errorf("pmConsultationBrief missing ready-stage protection clause; got %q", pmConsultationBrief)
	}
}

func TestPMLoop_ZeroIntervalNoOp(t *testing.T) {
	fake := &fakePMSpawner{}
	cfg := PMLoopConfig{
		TeamName: "test",
		Interval: 0,
		Spawner:  fake,
		Audit:    &captureSink{},
	}
	done := make(chan struct{})
	go func() {
		cfg.Loop(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Loop with Interval=0 did not return immediately")
	}
	if got := atomic.LoadInt32(&fake.spawnCalls); got != 0 {
		t.Errorf("Spawn called %d times with zero interval, want 0", got)
	}
}
