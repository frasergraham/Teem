package agent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/bus"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/roster"
	"github.com/frasergraham/teem/internal/team"
	"github.com/frasergraham/teem/internal/usage"
)

// throttledQuota always reports the throttle as active so we can assert
// the spawner refuses to provision.
type throttledQuota struct{}

func (throttledQuota) AvailableQuota(time.Time) (int64, int64, bool, string) {
	return 50, 100, true, "synthetic test cap"
}

func TestSpawner_RefusesWhenThrottled(t *testing.T) {
	tm := &team.Team{
		Name:   "x",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 1},
		},
	}
	reg := mcpsrv.NewRegistry()
	bs := bus.NewMemBus()
	t.Cleanup(func() { bs.Close() })
	r, err := roster.Open(filepath.Join(t.TempDir(), "roster.json"))
	if err != nil {
		t.Fatal(err)
	}
	sp := NewSpawner(context.Background(), tm, bs, reg, Config{
		Roster:     r,
		UsageQuota: throttledQuota{},
	})
	_, err = sp.Spawn(context.Background(), "worker", "ada")
	if err == nil {
		t.Fatalf("Spawn should refuse when quota gate trips")
	}
	if !errors.Is(err, usage.ErrUsageThrottle) {
		t.Errorf("err = %v, want errors.Is(usage.ErrUsageThrottle) true", err)
	}
}
