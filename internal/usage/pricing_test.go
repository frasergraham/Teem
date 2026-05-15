package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPricing_LoadFromFile_OK(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	body := []byte(`pricing:
  claude-opus-4-7:
    input_per_million: 15.00
    output_per_million: 75.00
    cache_read_per_million: 1.50
    cache_create_per_million: 18.75
  claude-sonnet-4-6:
    input_per_million: 3.00
    output_per_million: 15.00
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	p, ok, err := LoadPricing(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ok=false; want true")
	}
	if !p.HasPricing() {
		t.Fatal("HasPricing=false")
	}
	opus, ok := p.Models["claude-opus-4-7"]
	if !ok {
		t.Fatal("opus row missing")
	}
	if opus.InputPerMillion != 15 || opus.OutputPerMillion != 75 ||
		opus.CacheReadPerMillion != 1.5 || opus.CacheCreatePerMillion != 18.75 {
		t.Errorf("opus pricing wrong: %+v", opus)
	}
	if p.Stale {
		t.Errorf("freshly-written file should not be stale")
	}
}

func TestPricing_FileAbsent_HiddenUI(t *testing.T) {
	p, ok, err := LoadPricing(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("absent file should not error, got: %v", err)
	}
	if ok {
		t.Errorf("ok=true; want false for absent file")
	}
	if p.HasPricing() {
		t.Errorf("HasPricing=true on absent file; the UI would render $0 instead of hiding")
	}
}

func TestPricing_StaleWarn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	if err := os.WriteFile(path, []byte("pricing: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate mtime by StaleAge + 1d.
	old := time.Now().Add(-StaleAge - 24*time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	p, ok, err := LoadPricing(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ok=false")
	}
	if !p.Stale {
		t.Errorf("expected Stale=true for mtime %v (StaleAge=%v)", old, StaleAge)
	}
}

func TestPricing_MalformedYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	if err := os.WriteFile(path, []byte("pricing: [not a map]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, ok, err := LoadPricing(path)
	if err == nil {
		t.Fatal("expected parse error; got nil")
	}
	if ok {
		t.Errorf("ok=true on parse error; want false")
	}
}
