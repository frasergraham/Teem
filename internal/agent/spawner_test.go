package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/bus"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/roster"
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

// TestReconcileLocalSockets_AdoptsBareNameSocket guards the round-2
// blocker: a pre-canonicalisation worker (socket file named after the
// bare wordlist token, e.g. `ada.sock`) used to be silently abandoned
// because RoleFromID("ada") returns "". With the roster's bare→role
// lookup in place we now rename the on-disk artefacts to the
// canonical form and register the worker under its canonical id.
func TestReconcileLocalSockets_AdoptsBareNameSocket(t *testing.T) {
	// macOS caps unix socket paths around 104 chars; t.TempDir on
	// some runners exceeds that. Use a short /tmp path instead.
	socketDir, err := os.MkdirTemp("/tmp", "teem-bare-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	bareSocket := filepath.Join(socketDir, "ada.sock")
	barePid := filepath.Join(socketDir, "ada.pid")
	if err := os.WriteFile(barePid, []byte("12345\n"), 0o600); err != nil {
		t.Fatalf("write pid: %v", err)
	}

	// Start a unix-socket http server that answers /healthz like a
	// real teem-worker would.
	ln, err := net.Listen("unix", bareSocket)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" {
				http.NotFound(w, r)
				return
			}
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, "unauth", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		}),
	}
	go srv.Serve(ln)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	// Pre-seed the roster with the canonical entry that bare `ada`
	// would dedup onto. The reconcile path looks the bare name up
	// against this and recovers the role.
	r, err := roster.Open(filepath.Join(t.TempDir(), "roster.json"))
	if err != nil {
		t.Fatalf("roster open: %v", err)
	}
	if _, err := r.ReserveNamed("worker-ada", "worker"); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Roster.ReserveNamed leaves InUse=true; flip it back so the
	// reconcile path is the thing that marks it live.
	r.Release("worker-ada")

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
	sp := NewSpawner(context.Background(), tm, bs, reg, Config{
		WorkerToken: "tok",
		SocketDir:   socketDir,
		Roster:      r,
	})

	n := sp.ReconcileLocalSockets(context.Background())
	if n != 1 {
		t.Fatalf("ReconcileLocalSockets returned %d, want 1 (bare-name worker should be adopted)", n)
	}
	entry, ok := reg.Get("worker-ada")
	if !ok {
		t.Fatalf("worker-ada not registered after reconcile (bare-name socket was abandoned)")
	}
	if entry.State != mcpsrv.StateRunning {
		t.Errorf("worker-ada state = %q, want running", entry.State)
	}
	// On-disk artefacts should have been renamed to the canonical form.
	if _, err := os.Stat(filepath.Join(socketDir, "worker-ada.sock")); err != nil {
		t.Errorf("canonical socket missing after adopt: %v", err)
	}
	if _, err := os.Stat(bareSocket); err == nil {
		t.Errorf("bare socket %q should have been renamed away", bareSocket)
	}
	if _, err := os.Stat(filepath.Join(socketDir, "worker-ada.pid")); err != nil {
		t.Errorf("canonical pidfile missing after adopt: %v", err)
	}
	if _, err := os.Stat(barePid); err == nil {
		t.Errorf("bare pidfile %q should have been renamed away", barePid)
	}
	// Roster entry should now reflect the live worker.
	e, ok := r.Lookup("worker-ada")
	if !ok || !e.InUse {
		t.Errorf("roster entry worker-ada not marked InUse after reconcile: %+v", e)
	}
}
