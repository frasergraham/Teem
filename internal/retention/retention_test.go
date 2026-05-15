package retention

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfig_DefaultsToNever(t *testing.T) {
	// Make sure none of the retention env vars leak into the test.
	t.Setenv("TEEM_STOPPED_AGENT_TTL", "")
	t.Setenv("TEEM_TRANSCRIPT_TTL", "")
	t.Setenv("TEEM_RETENTION_SWEEP_INTERVAL", "")
	c := LoadConfig()
	if c.Enabled() {
		t.Fatalf("expected disabled default config; got %+v", c)
	}
	if c.StoppedAgentTTL != 0 || c.TranscriptTTL != 0 {
		t.Fatalf("default TTLs should be 0; got %+v", c)
	}
}

func TestLoadConfig_ParsesPositiveDurations(t *testing.T) {
	t.Setenv("TEEM_STOPPED_AGENT_TTL", "24h")
	t.Setenv("TEEM_TRANSCRIPT_TTL", "168h")
	t.Setenv("TEEM_RETENTION_SWEEP_INTERVAL", "10m")
	c := LoadConfig()
	if c.StoppedAgentTTL != 24*time.Hour {
		t.Errorf("StoppedAgentTTL: %v", c.StoppedAgentTTL)
	}
	if c.TranscriptTTL != 168*time.Hour {
		t.Errorf("TranscriptTTL: %v", c.TranscriptTTL)
	}
	if c.SweepInterval != 10*time.Minute {
		t.Errorf("SweepInterval: %v", c.SweepInterval)
	}
	if !c.Enabled() {
		t.Error("Enabled should be true with TTLs set")
	}
}

func TestLoadConfig_OffAndNeverDisable(t *testing.T) {
	for _, v := range []string{"off", "never", "0", "DISABLED"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("TEEM_STOPPED_AGENT_TTL", v)
			t.Setenv("TEEM_TRANSCRIPT_TTL", v)
			c := LoadConfig()
			if c.Enabled() {
				t.Errorf("%q should disable retention", v)
			}
		})
	}
}

func TestLoadConfig_BadValueFallsBackToDisabled(t *testing.T) {
	t.Setenv("TEEM_STOPPED_AGENT_TTL", "not-a-duration")
	c := LoadConfig()
	if c.StoppedAgentTTL != 0 {
		t.Errorf("invalid duration should resolve to 0; got %v", c.StoppedAgentTTL)
	}
}

func TestSweepTranscripts_TTLZeroIsNoOp(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "worker-ada", "j1.jsonl"), 24*time.Hour)
	removed, err := SweepTranscripts(dir, time.Now(), 0)
	if err != nil || removed != 0 {
		t.Fatalf("ttl=0 should be no-op; removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "worker-ada", "j1.jsonl")); err != nil {
		t.Errorf("file should still exist: %v", err)
	}
}

func TestSweepTranscripts_RemovesOnlyOldFiles(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "worker-ada", "old.jsonl")
	young := filepath.Join(dir, "worker-ada", "young.jsonl")
	writeFile(t, old, 48*time.Hour)     // 48h ago
	writeFile(t, young, 30*time.Minute) // 30 min ago

	removed, err := SweepTranscripts(dir, time.Now(), 24*time.Hour)
	if err != nil {
		t.Fatalf("sweep err: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed=%d, want 1", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old file should be gone: %v", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Errorf("young file should remain: %v", err)
	}
}

func TestSweepTranscripts_IgnoresNonJsonl(t *testing.T) {
	dir := t.TempDir()
	noise := filepath.Join(dir, "worker-ada", "README.txt")
	writeFile(t, noise, 365*24*time.Hour)
	removed, err := SweepTranscripts(dir, time.Now(), time.Hour)
	if err != nil || removed != 0 {
		t.Fatalf("non-jsonl shouldn't be swept; removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(noise); err != nil {
		t.Errorf("README.txt removed unexpectedly: %v", err)
	}
}

// writeFile creates path with empty contents and stamps its mtime to
// (now - age) so sweep tests can pretend it's old without actual sleep.
func writeFile(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(-age)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}
