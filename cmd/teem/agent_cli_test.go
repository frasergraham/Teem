package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frasergraham/teem/internal/archmem"
	"github.com/frasergraham/teem/internal/team"
)

const agentCLITestYAML = `
team:
  name: agentcli
  leader:
    system_prompt: "Ship the MVP."
  archetypes:
    - role: worker
      description: "Implements features."
      placement: local
      max_concurrent: 1
    - role: reviewer
      description: "Reviews diffs."
      placement: local
      max_concurrent: 1
`

// agentCLIFixture sets up a temp HOME and a team yaml in cwd so the
// CLI's defaults land under the test's scratch directory. The state
// dir component is now <t.ID> (not the team name), so we Load the
// team here to auto-mint and return the same id the runtime will see.
func agentCLIFixture(t *testing.T) (yamlPath, stateDir string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	yamlPath = filepath.Join(cwd, "teem.yaml")
	if err := os.WriteFile(yamlPath, []byte(agentCLITestYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	// Loading once mints + persists the id; subsequent loads (in the
	// CLI under test) read it back, so this id matches the runtime.
	tm, err := team.Load(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	stateDir = filepath.Join(home, ".teem", "state", tm.ID)
	return yamlPath, stateDir
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

func TestAgentList_IncludesLeaderAndArchetypes(t *testing.T) {
	agentCLIFixture(t)
	out := captureStdout(t, func() {
		if err := runAgentList(nil); err != nil {
			t.Fatalf("list: %v", err)
		}
	})
	for _, want := range []string{"leader", "worker", "reviewer", "NAME", "SOURCE"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in list output:\n%s", want, out)
		}
	}
}

func TestAgentShow_LeaderPrintsPromptAndMemory(t *testing.T) {
	agentCLIFixture(t)
	out := captureStdout(t, func() {
		if err := runAgentShow([]string{"leader"}); err != nil {
			t.Fatalf("show: %v", err)
		}
	})
	for _, want := range []string{"=== prompt ===", "Ship the MVP.", "=== memory ===", "no memory"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in show output:\n%s", want, out)
		}
	}
}

func TestAgentShow_PromptOnly(t *testing.T) {
	agentCLIFixture(t)
	out := captureStdout(t, func() {
		if err := runAgentShow([]string{"worker", "--prompt"}); err != nil {
			t.Fatalf("show: %v", err)
		}
	})
	if !strings.Contains(out, "Implements features.") {
		t.Errorf("prompt body missing:\n%s", out)
	}
	if strings.Contains(out, "=== memory ===") || strings.Contains(out, "=== prompt ===") {
		t.Errorf("single-flag show should not print headers:\n%s", out)
	}
}

func TestAgentShow_MemoryOnly(t *testing.T) {
	_, stateDir := agentCLIFixture(t)
	store := archmem.New(filepath.Join(stateDir, "memory"), nil)
	if err := store.AppendEntry("worker", archmem.Entry{
		AgentID: "worker-1", JobID: "j1", Status: "done", Summary: "touched x.go",
	}); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := runAgentShow([]string{"worker", "--memory"}); err != nil {
			t.Fatalf("show: %v", err)
		}
	})
	if !strings.Contains(out, "touched x.go") {
		t.Errorf("memory entry missing:\n%s", out)
	}
	if strings.Contains(out, "Implements features.") {
		t.Errorf("memory-only show should not include prompt:\n%s", out)
	}
}

func TestAgentShow_RejectsUnknownArchetype(t *testing.T) {
	agentCLIFixture(t)
	err := runAgentShow([]string{"ghost"})
	if err == nil {
		t.Fatal("expected error for unknown archetype")
	}
	if !strings.Contains(err.Error(), "valid choices") {
		t.Errorf("error should list valid choices: %v", err)
	}
}

func TestAgentShow_RejectsBadSlug(t *testing.T) {
	agentCLIFixture(t)
	for _, name := range []string{"../etc", "Worker", "with space"} {
		err := runAgentShow([]string{name})
		if err == nil {
			t.Errorf("expected error for %q", name)
		}
	}
}

func TestAgentUpdate_BothFlagsFails(t *testing.T) {
	agentCLIFixture(t)
	err := runAgentUpdate([]string{"reviewer", "--prompt", "--memory"})
	if err == nil {
		t.Fatal("expected error when both --prompt and --memory passed")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should explain mutual exclusion: %v", err)
	}
}

// TestAssembleLeaderBrief_FoldsLeaderMemory verifies the runChat prompt
// assembly prepends the leader memory file ahead of the team's brief
// when it exists, and leaves the brief untouched when empty. (Moved
// from the old memory_cli_test.go; the helper is in main.go.)
func TestAssembleLeaderBrief_FoldsLeaderMemory(t *testing.T) {
	_, stateDir := agentCLIFixture(t)
	memDir := filepath.Join(stateDir, "memory")
	tm, err := team.Load("teem.yaml")
	if err != nil {
		t.Fatal(err)
	}

	base := tm.LeaderSystemPrompt()
	got := assembleLeaderBrief(base, memDir)
	if strings.Contains(got, "Leader memory") {
		t.Errorf("empty leader memory should not inject section:\n%s", got)
	}

	store := archmem.New(memDir, nil)
	if err := store.AppendEntry(archmem.LeaderRole, archmem.Entry{
		AgentID: "leader", JobID: "", Status: "note", Summary: "T1 already shipped",
	}); err != nil {
		t.Fatal(err)
	}
	got = assembleLeaderBrief(base, memDir)
	if !strings.Contains(got, "# Leader memory (prior sessions)") {
		t.Errorf("missing leader memory header:\n%s", got)
	}
	if !strings.Contains(got, "T1 already shipped") {
		t.Errorf("leader memory body not folded in:\n%s", got)
	}
	// The memory section must precede the base brief so prior-session
	// context lands before the immutable team instructions.
	if mi, bi := strings.Index(got, "T1 already shipped"), strings.Index(got, "Ship the MVP."); mi < 0 || bi < 0 || mi > bi {
		t.Errorf("leader memory should appear before base brief; got memory@%d brief@%d:\n%s", mi, bi, got)
	}
}

func TestRunAgent_BareIsUsageNotError(t *testing.T) {
	out := captureStdout(t, func() {
		if err := runAgent(nil); err != nil {
			t.Fatalf("bare agent should not error: %v", err)
		}
	})
	if !strings.Contains(out, "teem agent — inspect and edit") {
		t.Errorf("expected usage on stdout, got:\n%s", out)
	}
}

func TestSplitEditor(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		err  bool
	}{
		{"vi", []string{"vi"}, false},
		{"code --wait", []string{"code", "--wait"}, false},
		{"emacs -nw  ", []string{"emacs", "-nw"}, false},
		{`"/Applications/My Editor/bin/ed" --flag`, []string{"/Applications/My Editor/bin/ed", "--flag"}, false},
		{`ed 'one two'`, []string{"ed", "one two"}, false},
		{`ed "unbalanced`, nil, true},
	}
	for _, c := range cases {
		got, err := splitEditor(c.in)
		if c.err {
			if err == nil {
				t.Errorf("splitEditor(%q) expected error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitEditor(%q): %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("splitEditor(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitEditor(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

// TestAgentUpdate_EditorNoop_SingleWord covers the common case: a
// single-word EDITOR (`true` succeeds without modifying the file). The
// CLI should print "no changes" and exit 0.
func TestAgentUpdate_EditorNoop_SingleWord(t *testing.T) {
	agentCLIFixture(t)
	t.Setenv("EDITOR", "true")
	t.Setenv("VISUAL", "")
	out := captureStdout(t, func() {
		if err := runAgentUpdate([]string{"worker", "--prompt"}); err != nil {
			t.Fatalf("update: %v", err)
		}
	})
	if !strings.Contains(out, "no changes") {
		t.Errorf("expected 'no changes' on unchanged content, got:\n%s", out)
	}
}

// TestAgentUpdate_EditorNoop_MultiWord locks in the issue-2 fix: a
// multi-word EDITOR like `code --wait` must shell-split so the OS
// doesn't try to exec a literal binary named "true --arg /dev/null".
func TestAgentUpdate_EditorNoop_MultiWord(t *testing.T) {
	agentCLIFixture(t)
	t.Setenv("EDITOR", "true --arg")
	t.Setenv("VISUAL", "")
	out := captureStdout(t, func() {
		if err := runAgentUpdate([]string{"worker", "--prompt"}); err != nil {
			t.Fatalf("update with multi-word EDITOR: %v", err)
		}
	})
	if !strings.Contains(out, "no changes") {
		t.Errorf("expected 'no changes', got:\n%s", out)
	}
}

// TestAgentUpdate_MemoryRequiresRecentEntriesHeader exercises the
// validate-then-write check from issue 3. We pre-populate the memory
// file with the canonical structure, then drive an "edit" that strips
// the `# Recent entries` header. Without --force the write must fail;
// with --force it must succeed.
func TestAgentUpdate_MemoryRequiresRecentEntriesHeader(t *testing.T) {
	_, stateDir := agentCLIFixture(t)
	memDir := filepath.Join(stateDir, "memory")
	store := archmem.New(memDir, nil)
	if err := store.AppendEntry("worker", archmem.Entry{
		AgentID: "w1", Status: "done", Summary: "seed",
	}); err != nil {
		t.Fatal(err)
	}

	// Write a "bad" temp body for the editor to "produce" — we mimic
	// the editor by writing a script that overwrites the temp file.
	scriptPath := filepath.Join(t.TempDir(), "fake-editor.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\ncat > \"$1\" <<'EOF'\n---\nrole: worker\n---\n# Digest\n(no recent entries section)\nEOF\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", scriptPath)
	t.Setenv("VISUAL", "")

	err := runAgentUpdate([]string{"worker", "--memory"})
	if err == nil {
		t.Fatal("expected error when edit drops '# Recent entries' header")
	}
	if !strings.Contains(err.Error(), "Recent entries") {
		t.Errorf("error should mention missing header: %v", err)
	}

	// --force lets it through.
	out := captureStdout(t, func() {
		if err := runAgentUpdate([]string{"worker", "--memory", "--force"}); err != nil {
			t.Errorf("--force should permit the write: %v", err)
		}
	})
	_ = out
}

// TestParsePositional_FlagWithSpaceSeparatedValue locks in issue 1:
// `--team /tmp/foo.yaml worker` should bind --team to the path, NOT
// pick up "/tmp/foo.yaml" as the positional and then have FlagSet
// silently set --team=worker.
func TestParsePositional_FlagWithSpaceSeparatedValue(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	teamPath := fs.String("team", "", "")
	got, err := parsePositional(fs, []string{"--team", "/tmp/foo.yaml", "worker"}, "show")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != "worker" {
		t.Errorf("positional = %q, want worker", got)
	}
	if *teamPath != "/tmp/foo.yaml" {
		t.Errorf("--team = %q, want /tmp/foo.yaml", *teamPath)
	}
}

func TestParsePositional_PositionalAnywhere(t *testing.T) {
	// Positional before flags.
	fs1 := flag.NewFlagSet("a", flag.ContinueOnError)
	p1 := fs1.Bool("prompt", false, "")
	got, err := parsePositional(fs1, []string{"worker", "--prompt"}, "show")
	if err != nil || got != "worker" || !*p1 {
		t.Errorf("before: got=%q err=%v prompt=%v", got, err, *p1)
	}
	// Positional after flags.
	fs2 := flag.NewFlagSet("b", flag.ContinueOnError)
	p2 := fs2.Bool("prompt", false, "")
	got, err = parsePositional(fs2, []string{"--prompt", "worker"}, "show")
	if err != nil || got != "worker" || !*p2 {
		t.Errorf("after: got=%q err=%v prompt=%v", got, err, *p2)
	}
}
