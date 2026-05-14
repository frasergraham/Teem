package team

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// driveWizard feeds the wizard a script of newline-separated lines and
// returns the produced YAML bytes (or an error). By default no CLAUDE.md
// is discovered — tests that exercise the fold path inject a stub finder
// via driveWizardWithFinder.
func driveWizard(t *testing.T, script string) ([]byte, error) {
	t.Helper()
	return driveWizardWithFinder(t, script, func() (string, string, bool) { return "", "", false })
}

func driveWizardWithFinder(t *testing.T, script string, finder func() (string, string, bool)) ([]byte, error) {
	t.Helper()
	in := strings.NewReader(script)
	var out bytes.Buffer
	z := NewWizard(in, &out)
	z.ClaudeMDFinder = finder
	_, body, err := z.Run()
	if err != nil {
		t.Logf("wizard output:\n%s", out.String())
		return nil, err
	}
	return body, nil
}

func TestWizard_OneLocalArchetype(t *testing.T) {
	// Custom-archetype flow: decline defaults, build one local
	// archetype, accept the default brief.
	script := strings.Join([]string{
		"example", // team name
		"n",       // use default archetypes? no
		"y",       // add an archetype?
		"worker",  // role
		"writes Go",
		"1", // placement = local
		"3", // max_concurrent
		"n", // add another?
		"n", // customize leader brief?
		"y", // write?
	}, "\n") + "\n"

	body, err := driveWizard(t, script)
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}

	got := loadFromBytes(t, body)
	if got.Name != "example" {
		t.Errorf("name = %q", got.Name)
	}
	if !strings.Contains(got.Leader.SystemPrompt, "leading a small team") {
		t.Errorf("default brief missing; got %q", got.Leader.SystemPrompt)
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
		"n", // decline default archetypes
		"y", // add an archetype
		"reviewer",
		"reads diffs",
		"2",                // ssh
		"alice@review-box", // ssh target
		"/home/alice/teem", // working_dir
		"2",                // max_concurrent
		"n",                // add another?
		"n",                // customize brief?
		"y",                // write?
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
		"n", // decline default archetypes
		"y", // add archetype
		"integrator",
		"",  // no description
		"3", // fargate
		"5", // max_concurrent
		"n", // add another?
		"n", // customize brief?
		"y", // write?
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
		"y", // use default archetypes
		"n", // customize brief?
		"n", // refuse to write
	}, "\n") + "\n"

	_, err := driveWizard(t, script)
	if err == nil || err != ErrCancelled {
		t.Fatalf("want ErrCancelled, got %v", err)
	}
}

func TestWizard_AcceptDefaults(t *testing.T) {
	// One-keystroke happy path: name, accept defaults, accept brief,
	// write.
	script := strings.Join([]string{
		"alpha",
		"y", // use default archetypes
		"n", // customize brief?
		"y", // write
	}, "\n") + "\n"

	body, err := driveWizard(t, script)
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}
	got := loadFromBytes(t, body)
	if got.Name != "alpha" {
		t.Errorf("name = %q", got.Name)
	}
	if len(got.Archetypes) != len(DefaultArchetypes) {
		t.Fatalf("want %d default archetypes, got %d", len(DefaultArchetypes), len(got.Archetypes))
	}
	for i, want := range DefaultArchetypes {
		if got.Archetypes[i].Role != want.Role {
			t.Errorf("archetype[%d].Role = %q, want %q", i, got.Archetypes[i].Role, want.Role)
		}
		if got.Archetypes[i].Placement != want.Placement {
			t.Errorf("archetype[%d].Placement = %q, want %q", i, got.Archetypes[i].Placement, want.Placement)
		}
		if got.Archetypes[i].MaxConcurrent != want.MaxConcurrent {
			t.Errorf("archetype[%d].MaxConcurrent = %d, want %d", i, got.Archetypes[i].MaxConcurrent, want.MaxConcurrent)
		}
	}
	if got.Leader.SystemPrompt != DefaultLeaderBrief {
		t.Errorf("leader brief != DefaultLeaderBrief; got %q", got.Leader.SystemPrompt)
	}
}

func TestWizard_FoldsClaudeMD(t *testing.T) {
	projectNotes := "# Project alpha\nUse goimports, not gofmt.\n"
	finder := func() (string, string, bool) {
		return "CLAUDE.md", projectNotes, true
	}
	script := strings.Join([]string{
		"alpha",
		"y", // fold CLAUDE.md
		"y", // use default archetypes
		"n", // customize brief?
		"y", // write
	}, "\n") + "\n"

	body, err := driveWizardWithFinder(t, script, finder)
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}
	got := loadFromBytes(t, body)
	if !strings.Contains(got.Leader.SystemPrompt, "Project specifics") {
		t.Errorf("leader brief missing CLAUDE.md section: %q", got.Leader.SystemPrompt)
	}
	if !strings.Contains(got.Leader.SystemPrompt, "Use goimports, not gofmt.") {
		t.Errorf("leader brief missing CLAUDE.md contents")
	}
}

func TestWizard_DeclinesClaudeMDFold(t *testing.T) {
	finder := func() (string, string, bool) {
		return "CLAUDE.md", "# project\nfoo\n", true
	}
	script := strings.Join([]string{
		"alpha",
		"n", // do NOT fold CLAUDE.md
		"y", // use default archetypes
		"n", // customize brief?
		"y", // write
	}, "\n") + "\n"

	body, err := driveWizardWithFinder(t, script, finder)
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}
	got := loadFromBytes(t, body)
	if strings.Contains(got.Leader.SystemPrompt, "Project specifics") {
		t.Errorf("leader brief should not include CLAUDE.md section: %q", got.Leader.SystemPrompt)
	}
	if got.Leader.SystemPrompt != DefaultLeaderBrief {
		t.Errorf("leader brief != DefaultLeaderBrief")
	}
}

func TestWizard_DeclinesDefaults_CustomFlow(t *testing.T) {
	script := strings.Join([]string{
		"beta",
		"n", // decline default archetypes
		"y", // add an archetype
		"worker",
		"writes Go",
		"1", // local
		"4", // max_concurrent
		"n", // no more archetypes
		"y", // customize brief
		"Custom brief line one.",
		"Custom brief line two.",
		"",  // end multiline
		"y", // write
	}, "\n") + "\n"

	body, err := driveWizard(t, script)
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}
	got := loadFromBytes(t, body)
	if len(got.Archetypes) != 1 || got.Archetypes[0].Role != "worker" || got.Archetypes[0].MaxConcurrent != 4 {
		t.Errorf("unexpected archetypes: %+v", got.Archetypes)
	}
	if !strings.Contains(got.Leader.SystemPrompt, "Custom brief line one.") {
		t.Errorf("custom brief missing: %q", got.Leader.SystemPrompt)
	}
	if strings.Contains(got.Leader.SystemPrompt, "leading a small team") {
		t.Errorf("custom brief should not include default text: %q", got.Leader.SystemPrompt)
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
