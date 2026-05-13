package team

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// driveWizard feeds the wizard a script of newline-separated lines and
// returns the produced YAML bytes (or an error).
func driveWizard(t *testing.T, script string) ([]byte, error) {
	t.Helper()
	in := strings.NewReader(script)
	var out bytes.Buffer
	z := NewWizard(in, &out)
	_, body, err := z.Run()
	if err != nil {
		t.Logf("wizard output:\n%s", out.String())
		return nil, err
	}
	return body, nil
}

func TestWizard_OneLocalArchetype(t *testing.T) {
	// Minimum viable team after the archetype migration: one
	// archetype is required by validation.
	script := strings.Join([]string{
		"example",                       // team name
		"You lead a team building Teem.", // leader prompt
		"",                              // end multiline
		"y",                             // add an archetype?
		"worker",                        // role
		"writes Go",                     // description
		"1",                             // placement = local
		"3",                             // max_concurrent
		"n",                             // add another? no
		"y",                             // write?
	}, "\n") + "\n"

	body, err := driveWizard(t, script)
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}

	got := loadFromBytes(t, body)
	if got.Name != "example" {
		t.Errorf("name = %q", got.Name)
	}
	if !strings.Contains(got.Leader.SystemPrompt, "Teem") {
		t.Errorf("leader prompt missing: %q", got.Leader.SystemPrompt)
	}
	if len(got.Archetypes) != 1 {
		t.Fatalf("want 1 archetype, got %d (yaml:\n%s)", len(got.Archetypes), body)
	}
	a := got.Archetypes[0]
	if a.Role != "worker" || a.Placement != "local" || a.MaxConcurrent != 3 {
		t.Errorf("unexpected archetype: %+v", a)
	}
}

func TestWizard_SSHArchetype(t *testing.T) {
	script := strings.Join([]string{
		"my-team",
		"leader brief",
		"",
		"y",
		"reviewer",
		"reads diffs",
		"2",                     // ssh
		"alice@review-box",      // ssh target
		"/home/alice/teem",      // working_dir
		"2",                     // max_concurrent
		"n",
		"y",
	}, "\n") + "\n"

	body, err := driveWizard(t, script)
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}
	got := loadFromBytes(t, body)
	if len(got.Archetypes) != 1 {
		t.Fatalf("want 1 archetype, got %d", len(got.Archetypes))
	}
	a := got.Archetypes[0]
	if a.Placement != "ssh:alice@review-box" || a.WorkingDir != "/home/alice/teem" || a.MaxConcurrent != 2 {
		t.Errorf("ssh archetype: %+v", a)
	}
}

func TestWizard_FargateArchetype(t *testing.T) {
	script := strings.Join([]string{
		"my-team",
		"leader",
		"",
		"y",
		"integrator",
		"",       // no description
		"3",      // fargate
		"5",      // max_concurrent
		"n",
		"y",
	}, "\n") + "\n"

	body, err := driveWizard(t, script)
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}
	got := loadFromBytes(t, body)
	if len(got.Archetypes) != 1 {
		t.Fatalf("want 1 archetype, got %d", len(got.Archetypes))
	}
	a := got.Archetypes[0]
	if a.Placement != "fargate" || a.MaxConcurrent != 5 {
		t.Errorf("fargate archetype: %+v", a)
	}
}

func TestWizard_Cancel(t *testing.T) {
	script := strings.Join([]string{
		"x",
		"x",
		"",
		"y",        // add archetype
		"worker",
		"",
		"1",
		"3",
		"n",
		"n",        // refuse to write
	}, "\n") + "\n"

	_, err := driveWizard(t, script)
	if err == nil || err != ErrCancelled {
		t.Fatalf("want ErrCancelled, got %v", err)
	}
}

func loadFromBytes(t *testing.T, body []byte) *Team {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "team.yaml")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	tm, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v\nyaml:\n%s", err, body)
	}
	return tm
}
