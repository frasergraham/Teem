package main

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/channelbus"
	"github.com/frasergraham/teem/internal/team"
)

// newDetectionTestTeam builds a minimal registeredTeam wired with a real
// channelbus and a real on-disk audit sink, but without Pulse/MCP/etc.
// observeChannelSubscribe only touches channelBus, detectionMu,
// channelsLive, auditSink, pulse (nil-tolerant), and team.ID — so the
// stub is sufficient to exercise the state machine.
func newDetectionTestTeam(t *testing.T) *registeredTeam {
	t.Helper()
	dir := t.TempDir()
	sink, err := audit.OpenFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	return &registeredTeam{
		team:       &team.Team{ID: "tst", Name: "test"},
		auditSink:  sink,
		channelBus: channelbus.New(4),
	}
}

func countChannelsState(t *testing.T, rt *registeredTeam, wantState string) int {
	t.Helper()
	events, err := rt.auditSink.Query("leader", time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, e := range events {
		if e.Kind != audit.KindChannelsState {
			continue
		}
		if state, _ := e.Meta["state"].(string); state == wantState {
			got++
		}
	}
	return got
}

// TestObserveChannelSubscribe_FirstSubscriberEmitsLive: a single
// subscribe must produce exactly one channels_state=live audit event,
// and the per-team flag must be set.
func TestObserveChannelSubscribe_FirstSubscriberEmitsLive(t *testing.T) {
	d := &daemon{}
	rt := newDetectionTestTeam(t)

	_, cancel := d.observeChannelSubscribe(rt)
	defer cancel()

	if !rt.channelsLive {
		t.Error("channelsLive should be true after first subscribe")
	}
	if got := countChannelsState(t, rt, "live"); got != 1 {
		t.Errorf("expected 1 live audit event, got %d", got)
	}
}

// TestObserveChannelSubscribe_LastUnsubscribeEmitsFallback: after the
// last subscriber leaves, exactly one channels_state=fallback event
// fires and the flag clears.
func TestObserveChannelSubscribe_LastUnsubscribeEmitsFallback(t *testing.T) {
	d := &daemon{}
	rt := newDetectionTestTeam(t)

	_, cancel := d.observeChannelSubscribe(rt)
	cancel()

	if rt.channelsLive {
		t.Error("channelsLive should be false after last unsubscribe")
	}
	if got := countChannelsState(t, rt, "fallback"); got != 1 {
		t.Errorf("expected 1 fallback audit event, got %d", got)
	}
}

// TestObserveChannelSubscribe_ConcurrentEmitsExactlyOneLive: N
// concurrent subscribers must produce exactly one live transition (and
// later exactly one fallback when all leave). This is the property the
// daemon relies on — without holding detectionMu across the channelbus
// op and the flag mutation, two near-simultaneous SSE connections
// would each see "first subscriber" via Subscribe+Len and emit a live
// event each.
func TestObserveChannelSubscribe_ConcurrentEmitsExactlyOneLive(t *testing.T) {
	const N = 16
	d := &daemon{}
	rt := newDetectionTestTeam(t)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		cancels []func()
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, c := d.observeChannelSubscribe(rt)
			mu.Lock()
			cancels = append(cancels, c)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if got := countChannelsState(t, rt, "live"); got != 1 {
		t.Errorf("expected exactly 1 live event under concurrent subscribes, got %d", got)
	}
	if !rt.channelsLive {
		t.Error("channelsLive should be true while subscribers are connected")
	}

	// Cancel all concurrently; exactly one fallback should fire.
	var cwg sync.WaitGroup
	cwg.Add(len(cancels))
	for _, c := range cancels {
		c := c
		go func() {
			defer cwg.Done()
			c()
		}()
	}
	cwg.Wait()

	if got := countChannelsState(t, rt, "fallback"); got != 1 {
		t.Errorf("expected exactly 1 fallback event under concurrent cancels, got %d", got)
	}
	if rt.channelsLive {
		t.Error("channelsLive should be false after last unsubscribe")
	}
}

// TestObserveChannelSubscribe_ReconnectCycleFiresBothTransitions:
// subscribe → cancel → subscribe → cancel must emit (live, fallback,
// live, fallback) — every transition observed, no flapping suppression
// in v1.
func TestObserveChannelSubscribe_ReconnectCycleFiresBothTransitions(t *testing.T) {
	d := &daemon{}
	rt := newDetectionTestTeam(t)

	for i := 0; i < 2; i++ {
		_, c := d.observeChannelSubscribe(rt)
		c()
	}
	if got := countChannelsState(t, rt, "live"); got != 2 {
		t.Errorf("expected 2 live events across two cycles, got %d", got)
	}
	if got := countChannelsState(t, rt, "fallback"); got != 2 {
		t.Errorf("expected 2 fallback events across two cycles, got %d", got)
	}
}

// TestObserveChannelSubscribe_NoFlapMidStream: with two overlapping
// subscribers, neither connect nor disconnect of the second should
// emit any extra channels_state events — the flag is already live.
func TestObserveChannelSubscribe_NoFlapMidStream(t *testing.T) {
	d := &daemon{}
	rt := newDetectionTestTeam(t)

	_, c1 := d.observeChannelSubscribe(rt)
	_, c2 := d.observeChannelSubscribe(rt)
	c2()
	if got := countChannelsState(t, rt, "live"); got != 1 {
		t.Errorf("after subscribe+subscribe+cancel: expected 1 live, got %d", got)
	}
	if got := countChannelsState(t, rt, "fallback"); got != 0 {
		t.Errorf("after subscribe+subscribe+cancel: expected 0 fallback, got %d", got)
	}
	c1()
	if got := countChannelsState(t, rt, "fallback"); got != 1 {
		t.Errorf("after full teardown: expected 1 fallback, got %d", got)
	}
}
