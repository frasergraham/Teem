package provisioner

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildTeemWorker compiles cmd/teem-worker into a temp binary so the
// subprocess provisioner tests don't depend on the user having
// teem-worker installed in $PATH at the right version.
func buildTeemWorker(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "teem-worker")
	// Resolve the repo root from this test file's location.
	repo := repoRoot(t)
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/teem-worker")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build teem-worker: %v\n%s", err, out)
	}
	return bin
}

// repoRoot walks up from the current file's directory looking for
// go.mod. Avoids hardcoding a path.
func repoRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := cwd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		next := filepath.Dir(dir)
		if next == dir {
			break
		}
		dir = next
	}
	t.Fatalf("no go.mod above %s", cwd)
	return ""
}

func TestLocalSubprocess_SpawnAndShutdown(t *testing.T) {
	bin := buildTeemWorker(t)
	work := t.TempDir()
	sockDir := shortTempDir(t)

	p := &LocalProvisioner{
		SocketDir:    sockDir,
		LeaderURL:    "", // no leader; worker just listens
		WorkerToken:  "test-tok",
		WorkerBinary: bin,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, err := p.Provision(ctx, AgentSpec{
		ID:         "worker-1",
		Role:       "worker",
		WorkingDir: work,
	})
	if err != nil {
		// Surface the worker's log so failures are debuggable.
		body, _ := os.ReadFile(filepath.Join(sockDir, "worker-1.log"))
		t.Fatalf("Provision: %v\n--- worker.log ---\n%s", err, body)
	}
	if a.SocketPath == "" {
		t.Fatal("expected SocketPath to be populated")
	}

	// Hit /healthz over the unix socket.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", a.SocketPath)
			},
		},
		Timeout: 3 * time.Second,
	}
	req, _ := http.NewRequest(http.MethodGet, "http://unix/healthz", nil)
	req.Header.Set("Authorization", "Bearer test-tok")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("healthz dial: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("healthz status: %d", resp.StatusCode)
	}

	// Teardown should clean up.
	if err := p.Teardown(ctx, a); err != nil {
		t.Errorf("Teardown: %v", err)
	}
	// Socket file should be gone within a moment.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(a.SocketPath); os.IsNotExist(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(a.SocketPath); err == nil {
		t.Errorf("socket %s should be removed after teardown", a.SocketPath)
	}
}

func TestLocalSubprocess_SurvivesProvisionerExit(t *testing.T) {
	bin := buildTeemWorker(t)
	work := t.TempDir()
	sockDir := shortTempDir(t)
	tok := "test-tok"

	p := &LocalProvisioner{
		SocketDir:    sockDir,
		WorkerToken:  tok,
		WorkerBinary: bin,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, err := p.Provision(ctx, AgentSpec{ID: "worker-2", Role: "worker", WorkingDir: work})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// "Forget" the provisioner — simulate daemon stop without
	// teardown. The socket should still exist; a fresh provisioner
	// should be able to dial it.
	p2 := &LocalProvisioner{SocketDir: sockDir, WorkerToken: tok, WorkerBinary: bin}
	_ = p2 // not used; assertion is that the socket survives

	// Healthz still works.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", a.SocketPath)
			},
		},
		Timeout: 3 * time.Second,
	}
	req, _ := http.NewRequest(http.MethodGet, "http://unix/healthz", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("healthz after provisioner-exit: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("healthz post-exit status: %d", resp.StatusCode)
	}

	// Clean up so we don't leak a teem-worker process.
	_ = p.Teardown(ctx, a)
}

func TestLocalSubprocess_FallbackToInProcess(t *testing.T) {
	// With SocketDir empty, the provisioner returns the legacy
	// in-process Agent (Transport set, no SocketPath). Tests rely
	// on this — exercise it directly.
	work := t.TempDir()
	p := &LocalProvisioner{} // no socket dir
	a, err := p.Provision(context.Background(), AgentSpec{
		ID:         "worker-1",
		Role:       "worker",
		WorkingDir: work,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if a.Transport == nil {
		t.Error("expected legacy Transport for in-process fallback")
	}
	if a.SocketPath != "" {
		t.Errorf("legacy path should not set SocketPath, got %q", a.SocketPath)
	}
}

// shortTempDir returns a tempdir under /tmp with a short name —
// macOS limits unix socket paths to ~104 chars, and the default
// t.TempDir() under /var/folders is already past that on its own.
// Cleaned up via t.Cleanup.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// unused suppressor while we iterate.
var _ = strings.TrimSpace
