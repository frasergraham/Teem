package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/bus"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/team"
)

// rerouteTransport sends every request to a fixed httptest.Server,
// regardless of the original URL host. This lets us point the Spawner's
// "tailnet HTTPClient" at a local fake without standing up tsnet.
type rerouteTransport struct{ target string }

func (r rerouteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = r.target
	return http.DefaultTransport.RoundTrip(req)
}

func TestSpawner_Reconcile_RegistersPersistentLocalAgent(t *testing.T) {
	// Fake worker that answers /healthz with the same auth as a real one.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauth", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()
	target := strings.TrimPrefix(ts.URL, "http://")

	tm := &team.Team{
		Name:   "x",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "background", Placement: "local", MaxConcurrent: 1, Lifecycle: "persistent"},
			{Role: "backend", Placement: "local", MaxConcurrent: 1}, // ephemeral; should be ignored
		},
	}

	reg := mcpsrv.NewRegistry()
	bs := bus.NewMemBus()
	defer bs.Close()

	s := NewSpawner(context.Background(), tm, bs, reg, Config{
		HTTPClient:  &http.Client{Transport: rerouteTransport{target: target}, Timeout: 2 * time.Second},
		WorkerToken: "tok",
	})

	n := s.Reconcile(context.Background())
	if n != 1 {
		t.Fatalf("Reconcile returned %d, want 1", n)
	}
	// Persistent archetype with one instance probes teem-background-1.
	entry, ok := reg.Get("background-1")
	if !ok {
		t.Fatalf("background-1 not registered after Reconcile")
	}
	if entry.State != mcpsrv.StateRunning {
		t.Errorf("background-1 state = %q, want running", entry.State)
	}
	if _, ok := reg.Get("backend-1"); ok {
		t.Errorf("ephemeral backend archetype should not be reconciled")
	}
}

func TestSpawner_Reconcile_SkipsUnreachable(t *testing.T) {
	// httptest with a 503 health endpoint so reconcile sees the agent as down.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	target := strings.TrimPrefix(ts.URL, "http://")
	tm := &team.Team{
		Name:   "x",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "background", Placement: "local", MaxConcurrent: 1, Lifecycle: "persistent"},
		},
	}
	reg := mcpsrv.NewRegistry()
	bs := bus.NewMemBus()
	defer bs.Close()
	s := NewSpawner(context.Background(), tm, bs, reg, Config{
		HTTPClient:  &http.Client{Transport: rerouteTransport{target: target}, Timeout: 2 * time.Second},
		WorkerToken: "tok",
	})
	if n := s.Reconcile(context.Background()); n != 0 {
		t.Fatalf("Reconcile returned %d, want 0", n)
	}
	if _, ok := reg.Get("background-1"); ok {
		t.Errorf("unreachable agent should not be registered")
	}
}

// guard against the "no http client" early-return: should be a quiet no-op,
// not a panic.
func TestSpawner_Reconcile_NoHTTPClient(t *testing.T) {
	tm := &team.Team{
		Name:   "x",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "background", Placement: "local", MaxConcurrent: 1, Lifecycle: "persistent"},
		},
	}
	reg := mcpsrv.NewRegistry()
	bs := bus.NewMemBus()
	defer bs.Close()
	s := NewSpawner(context.Background(), tm, bs, reg, Config{})
	if n := s.Reconcile(context.Background()); n != 0 {
		t.Errorf("Reconcile w/o http client returned %d, want 0", n)
	}
}

// satisfy the linter for the unused import in some toolchains
var _ = net.Listen
