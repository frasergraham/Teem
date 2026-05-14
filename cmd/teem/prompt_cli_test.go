package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const promptTestYAML = `
team:
  name: prompt-cli-test
  leader:
    system_prompt: "Ship the MVP."
  archetypes:
    - role: worker
      description: "Implements features."
      placement: local
      max_concurrent: 1
`

func writePromptTestTeam(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "team.yaml")
	if err := os.WriteFile(path, []byte(promptTestYAML), 0o600); err != nil {
		t.Fatalf("write team: %v", err)
	}
	// Isolate HOME so defaultPromptOverrideDir lands in t.TempDir().
	t.Setenv("HOME", t.TempDir())
	return path
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	fn()
	_ = w.Close()
	<-done
	os.Stdout = orig
	return buf.String()
}

func TestPromptShow_Leader(t *testing.T) {
	teamPath := writePromptTestTeam(t)
	out := captureStdout(t, func() {
		if err := runPromptShow([]string{"--role", "leader", "--team", teamPath}); err != nil {
			t.Fatalf("show: %v", err)
		}
	})
	for _, want := range []string{"You are the Leader", "worker", "Ship the MVP."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in show output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Operator overrides") {
		t.Errorf("no overrides expected; output:\n%s", out)
	}
}

func TestPromptAppendThenShow_Worker(t *testing.T) {
	teamPath := writePromptTestTeam(t)
	if err := runPromptAppend([]string{"--role", "worker", "--team", teamPath, "always", "run", "go", "vet"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	out := captureStdout(t, func() {
		if err := runPromptShow([]string{"--role", "worker", "--team", teamPath}); err != nil {
			t.Fatalf("show: %v", err)
		}
	})
	for _, want := range []string{"Implements features.", "Operator overrides", "always run go vet"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in worker show output:\n%s", want, out)
		}
	}
}

func TestPromptShow_RawOnlyOverride(t *testing.T) {
	teamPath := writePromptTestTeam(t)
	if err := runPromptAppend([]string{"--role", "worker", "--team", teamPath, "raw-mode-marker"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	out := captureStdout(t, func() {
		if err := runPromptShow([]string{"--role", "worker", "--team", teamPath, "--raw"}); err != nil {
			t.Fatalf("show: %v", err)
		}
	})
	if !strings.Contains(out, "raw-mode-marker") {
		t.Errorf("raw output missing marker:\n%s", out)
	}
	if strings.Contains(out, "Implements features.") {
		t.Errorf("raw output should not include YAML-derived base:\n%s", out)
	}
}

func TestPromptShow_RejectsUnknownRole(t *testing.T) {
	teamPath := writePromptTestTeam(t)
	err := runPromptShow([]string{"--role", "ghost", "--team", teamPath})
	if err == nil {
		t.Errorf("expected error for unknown role")
	}
}
