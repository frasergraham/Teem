package team

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// driveWizard feeds the wizard a script of newline-separated lines and
// returns the produced YAML bytes (or an error). Useful for asserting the
// wizard's YAML output round-trips through team.Load.
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

func TestWizard_LeaderOnly(t *testing.T) {
	// Minimum viable team: name + leader prompt, no agents.
	script := strings.Join([]string{
		"example",       // team name
		"You lead a team building Teem.", // leader prompt line 1
		"",              // end of multiline
		"n",             // add an agent? no
		"y",             // write?
	}, "\n") + "\n"

	body, err := driveWizard(t, script)
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}

	// Round-trip the produced YAML through team.Load.
	dir := t.TempDir()
	path := filepath.Join(dir, "team.yaml")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v\nyaml:\n%s", err, body)
	}
	if got.Name != "example" {
		t.Errorf("name = %q", got.Name)
	}
	if !strings.Contains(got.Leader.SystemPrompt, "Teem") {
		t.Errorf("leader prompt missing: %q", got.Leader.SystemPrompt)
	}
	if len(got.Agents) != 0 {
		t.Errorf("agents: %d", len(got.Agents))
	}
}

func TestWizard_LocalAgent(t *testing.T) {
	script := strings.Join([]string{
		"my-team",
		"You are the leader.",
		"",
		"y",              // add agent
		"backend",        // role
		"",               // id default
		"writes Go",      // description
		"1",              // local
		"n",              // no more agents
		"y",              // write
	}, "\n") + "\n"

	body, err := driveWizard(t, script)
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "team.yaml")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v\nyaml:\n%s", err, body)
	}
	if len(got.Agents) != 1 {
		t.Fatalf("want 1 agent, got %d (yaml:\n%s)", len(got.Agents), body)
	}
	a := got.Agents[0]
	if a.ID != "backend-1" || a.Role != "backend" || !a.Local || a.SSHTarget != "" || a.Backend != "" {
		t.Errorf("unexpected agent: %+v", a)
	}
}

func TestWizard_SSHAgent(t *testing.T) {
	script := strings.Join([]string{
		"my-team",
		"leader brief",
		"",
		"y",
		"reviewer",
		"rv-1",                  // explicit id
		"reads diffs",           // description
		"2",                     // ssh
		"alice@review-box",      // ssh target
		"/home/alice/teem",      // working_dir
		"n",
		"y",
	}, "\n") + "\n"

	body, err := driveWizard(t, script)
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}
	got, err := loadFromBytes(t, body)
	if err != nil {
		t.Fatalf("load: %v\nyaml:\n%s", err, body)
	}
	a := got.Agents[0]
	if a.SSHTarget != "alice@review-box" || a.WorkingDir != "/home/alice/teem" || a.Local {
		t.Errorf("ssh agent: %+v", a)
	}
}

func TestWizard_FargateAgent(t *testing.T) {
	script := strings.Join([]string{
		"my-team",
		"leader",
		"",
		"y",
		"integrator",
		"",       // default id
		"",       // no description
		"3",      // fargate
		"n",
		"y",
	}, "\n") + "\n"

	body, err := driveWizard(t, script)
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}
	got, err := loadFromBytes(t, body)
	if err != nil {
		t.Fatalf("load: %v\nyaml:\n%s", err, body)
	}
	a := got.Agents[0]
	if a.Backend != "fargate" || a.Local || a.SSHTarget != "" {
		t.Errorf("fargate agent: %+v", a)
	}
}

func TestWizard_Cancel(t *testing.T) {
	script := strings.Join([]string{
		"x",
		"x",
		"",
		"n",
		"n", // refuse to write
	}, "\n") + "\n"

	_, err := driveWizard(t, script)
	if err == nil || err != ErrCancelled {
		t.Fatalf("want ErrCancelled, got %v", err)
	}
}

func loadFromBytes(t *testing.T, body []byte) (*Team, error) {
	dir := t.TempDir()
	path := filepath.Join(dir, "team.yaml")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}
