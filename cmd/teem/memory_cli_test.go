package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frasergraham/teem/internal/archmem"
	"github.com/frasergraham/teem/internal/team"
)

const memoryCLITestYAML = `
team:
  name: memcli
  tailnet:
    hostname: memcli
    auth_key_env: TS_AUTHKEY
  leader:
    system_prompt: "be the leader."
    mcps: []
  archetypes:
    - role: worker
      description: "the worker."
      placement: local
      max_concurrent: 1
`

// memoryCLIFixture sets up a temp HOME and a team yaml in cwd so the
// CLI's defaultMemoryDir + resolveTeamPath both land under the test's
// scratch directory.
func memoryCLIFixture(t *testing.T) (yamlPath, memDir string) {
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
	if err := os.WriteFile(yamlPath, []byte(memoryCLITestYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	memDir = filepath.Join(home, ".teem", "state", "memcli", "memory")
	return yamlPath, memDir
}

func TestMemoryCLI_AppendThenShow_Worker(t *testing.T) {
	_, memDir := memoryCLIFixture(t)
	if err := runMemory([]string{"append", "--role", "worker", "always run gofmt"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(memDir, "worker.md"))
	if err != nil {
		t.Fatalf("read worker.md: %v", err)
	}
	if !strings.Contains(string(body), "always run gofmt") {
		t.Errorf("worker.md missing note:\n%s", body)
	}
	if !strings.Contains(string(body), "operator") {
		t.Errorf("appended note should attribute to operator:\n%s", body)
	}
}

func TestMemoryCLI_LeaderRoleAccepted(t *testing.T) {
	_, memDir := memoryCLIFixture(t)
	if err := runMemory([]string{"append", "--role", "leader", "T7 deferred to next week"}); err != nil {
		t.Fatalf("append leader: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(memDir, "leader.md"))
	if err != nil {
		t.Fatalf("read leader.md: %v", err)
	}
	if !strings.Contains(string(body), "T7 deferred") {
		t.Errorf("leader.md missing note:\n%s", body)
	}
	if !strings.Contains(string(body), "role: leader") {
		t.Errorf("frontmatter role missing:\n%s", body)
	}
}

func TestMemoryCLI_RejectsUnknownRole(t *testing.T) {
	memoryCLIFixture(t)
	err := runMemory([]string{"append", "--role", "ghost", "x"})
	if err == nil {
		t.Fatal("expected error for unknown role")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should mention the role: %v", err)
	}
}

func TestMemoryCLI_RejectsBadRoleSlug(t *testing.T) {
	memoryCLIFixture(t)
	for _, role := range []string{"../etc", "Worker", "with space"} {
		err := runMemory([]string{"append", "--role", role, "x"})
		if err == nil {
			t.Errorf("expected error for role %q", role)
		}
	}
}

func TestMemoryShowHelper(t *testing.T) {
	_, memDir := memoryCLIFixture(t)
	store := archmem.New(memDir, nil)
	if err := store.AppendEntry("worker", archmem.Entry{
		AgentID: "worker-1", JobID: "j1", Status: "done", Summary: "did the thing",
	}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := memoryShow(store, "worker", &buf); err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(buf.String(), "did the thing") {
		t.Errorf("show output missing entry:\n%s", buf.String())
	}
}

func TestMemoryShowHelper_EmptyRolePrintsPlaceholder(t *testing.T) {
	_, memDir := memoryCLIFixture(t)
	store := archmem.New(memDir, nil)
	var buf bytes.Buffer
	if err := memoryShow(store, "worker", &buf); err != nil {
		t.Fatalf("show empty: %v", err)
	}
	if !strings.Contains(buf.String(), "no memory") {
		t.Errorf("expected placeholder, got:\n%s", buf.String())
	}
}

// TestAssembleLeaderBrief_FoldsLeaderMemory verifies the runChat
// prompt assembly prepends the leader memory file ahead of the team's
// brief when it exists, and leaves the brief untouched when empty.
func TestAssembleLeaderBrief_FoldsLeaderMemory(t *testing.T) {
	_, memDir := memoryCLIFixture(t)
	tm, err := team.Load("teem.yaml")
	if err != nil {
		t.Fatal(err)
	}

	// Empty case: brief is unchanged.
	got := assembleLeaderBrief(tm, memDir)
	if strings.Contains(got, "Leader memory") {
		t.Errorf("empty leader memory should not inject section:\n%s", got)
	}

	// Seed leader memory and re-assemble.
	store := archmem.New(memDir, nil)
	if err := store.AppendEntry(archmem.LeaderRole, archmem.Entry{
		AgentID: "leader", JobID: "", Status: "note", Summary: "T1 already shipped",
	}); err != nil {
		t.Fatal(err)
	}
	got = assembleLeaderBrief(tm, memDir)
	if !strings.Contains(got, "# Leader memory (prior sessions)") {
		t.Errorf("missing leader memory header:\n%s", got)
	}
	if !strings.Contains(got, "T1 already shipped") {
		t.Errorf("leader memory body not folded in:\n%s", got)
	}
	// The team's standard brief must follow.
	if !strings.Contains(got, "be the leader.") {
		t.Errorf("team brief missing after leader memory:\n%s", got)
	}
	if strings.Index(got, "T1 already shipped") > strings.Index(got, "be the leader.") {
		t.Errorf("leader memory should appear BEFORE the team brief:\n%s", got)
	}
}
