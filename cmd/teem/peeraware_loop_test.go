package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/leaderstatus"
	"github.com/frasergraham/teem/internal/team"
)

func TestParsePeerAwareInterval(t *testing.T) {
	type want struct {
		dur      time.Duration
		warned   bool
		warnHint string
	}
	cases := []struct {
		name string
		raw  string
		want want
	}{
		{name: "empty defaults to 1h", raw: "", want: want{dur: peerAwareDefaultInterval}},
		{name: "30m", raw: "30m", want: want{dur: 30 * time.Minute}},
		{name: "zero disables", raw: "0", want: want{dur: 0}},
		{name: "off disables", raw: "off", want: want{dur: 0}},
		{name: "never disables", raw: "never", want: want{dur: 0}},
		{name: "disabled disables", raw: "disabled", want: want{dur: 0}},
		{name: "uppercase OFF disables", raw: "OFF", want: want{dur: 0}},
		{name: "whitespace + 5m", raw: "  5m  ", want: want{dur: 5 * time.Minute}},
		{name: "garbage falls back + warns", raw: "garbage", want: want{dur: peerAwareDefaultInterval, warned: true, warnHint: `"garbage"`}},
		{name: "negative duration falls back + warns", raw: "-1h", want: want{dur: peerAwareDefaultInterval, warned: true, warnHint: `"-1h"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			logf := func(format string, args ...any) {
				fmt.Fprintf(&buf, format, args...)
			}
			got := parsePeerAwareInterval(tc.raw, logf)
			if got != tc.want.dur {
				t.Errorf("dur: got %v, want %v", got, tc.want.dur)
			}
			warned := buf.Len() > 0
			if warned != tc.want.warned {
				t.Errorf("warned=%v, want %v (buf=%q)", warned, tc.want.warned, buf.String())
			}
			if tc.want.warnHint != "" && !strings.Contains(buf.String(), tc.want.warnHint) {
				t.Errorf("warning missing hint %q; got: %q", tc.want.warnHint, buf.String())
			}
		})
	}
}

// TestCollectSnapshot_SkipsStaleLeaderStatus verifies that a leader
// status whose UpdatedAt is older than the window doesn't get copied
// into the snapshot — otherwise an idle leader from many hours ago
// would re-trip a fresh peer block on every hourly tick.
func TestCollectSnapshot_SkipsStaleLeaderStatus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leader_status.json")
	stale := time.Now().UTC().Add(-2 * time.Hour)
	seed := map[string]leaderstatus.Entry{
		"leader": {
			AgentID:   "leader",
			Text:      "looked busy two hours ago",
			UpdatedAt: stale,
		},
	}
	body, err := json.Marshal(seed)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	ls, err := leaderstatus.Open(path)
	if err != nil {
		t.Fatalf("open leaderstatus: %v", err)
	}
	if e, ok := ls.Get("leader"); !ok || e.UpdatedAt != stale {
		t.Fatalf("seed not loaded: ok=%v entry=%+v", ok, e)
	}

	rt := &registeredTeam{
		team:         &team.Team{Name: "stale-team"},
		leaderStatus: ls,
	}
	cutoff := time.Now().UTC().Add(-time.Hour) // 1h window
	snap := collectSnapshot(rt, cutoff)
	if snap.LeaderStatus != "" {
		t.Errorf("LeaderStatus = %q, want empty (stale)", snap.LeaderStatus)
	}
	if !snap.LeaderUpdated.IsZero() {
		t.Errorf("LeaderUpdated = %v, want zero (stale)", snap.LeaderUpdated)
	}

	// Sanity: a fresh status (within the window) should be copied through.
	fresh := time.Now().UTC().Add(-15 * time.Minute)
	freshSeed := map[string]leaderstatus.Entry{
		"leader": {
			AgentID:   "leader",
			Text:      "working on T7",
			UpdatedAt: fresh,
		},
	}
	freshBody, err := json.Marshal(freshSeed)
	if err != nil {
		t.Fatalf("marshal fresh seed: %v", err)
	}
	freshPath := filepath.Join(dir, "leader_status_fresh.json")
	if err := os.WriteFile(freshPath, freshBody, 0o600); err != nil {
		t.Fatalf("write fresh seed: %v", err)
	}
	freshLS, err := leaderstatus.Open(freshPath)
	if err != nil {
		t.Fatalf("open fresh leaderstatus: %v", err)
	}
	rt.leaderStatus = freshLS
	snap = collectSnapshot(rt, cutoff)
	if snap.LeaderStatus != "working on T7" {
		t.Errorf("LeaderStatus = %q, want %q", snap.LeaderStatus, "working on T7")
	}
	if !snap.LeaderUpdated.Equal(fresh) {
		t.Errorf("LeaderUpdated = %v, want %v", snap.LeaderUpdated, fresh)
	}
}

// TestPeerAwareConfig_HonoursEnv verifies the env-reading wrapper end
// to end via t.Setenv; the parse logic itself is covered by the table
// test above.
func TestPeerAwareConfig_HonoursEnv(t *testing.T) {
	t.Setenv("TEEM_PEERAWARE_INTERVAL", "15m")
	if got := peerAwareConfig(); got != 15*time.Minute {
		t.Errorf("got %v, want 15m", got)
	}
	t.Setenv("TEEM_PEERAWARE_INTERVAL", "off")
	if got := peerAwareConfig(); got != 0 {
		t.Errorf("got %v, want 0 (disabled)", got)
	}
}
