package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "team.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoad_Valid(t *testing.T) {
	path := writeTemp(t, `
team:
  name: alpha
  tailnet:
    hostname: alpha-leader
    auth_key_env: TS_AUTHKEY
  leader:
    system_prompt: "Ship the MVP."
  archetypes:
    - role: worker
      description: "Implements features."
      placement: local
      max_concurrent: 5
    - role: reviewer
      description: "Reads diffs."
      placement: local
      max_concurrent: 3
`)
	team, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if team.Name != "alpha" {
		t.Errorf("name: got %q", team.Name)
	}
	if team.Tailnet.Hostname != "alpha-leader" {
		t.Errorf("hostname: got %q", team.Tailnet.Hostname)
	}
	if len(team.Archetypes) != 2 {
		t.Fatalf("archetypes: got %d", len(team.Archetypes))
	}
	prompt := team.LeaderSystemPrompt()
	for _, want := range []string{"worker", "reviewer", "Ship the MVP."} {
		if !strings.Contains(prompt, want) {
			t.Errorf("LeaderSystemPrompt missing %q", want)
		}
	}
}

func TestLoad_DefaultsHostname(t *testing.T) {
	path := writeTemp(t, `
team:
  name: "Big Cats!"
  leader:
    system_prompt: "x"
  archetypes:
    - role: worker
      placement: local
      max_concurrent: 1
`)
	team, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if team.Tailnet.Hostname != "big-cats" {
		t.Errorf("hostname: got %q", team.Tailnet.Hostname)
	}
}

func TestDefaultLeaderBrief_RendersInSystemPrompt(t *testing.T) {
	team := &Team{
		Name:       "alpha",
		Leader:     LeaderSpec{SystemPrompt: DefaultLeaderBrief},
		Archetypes: cloneArchetypes(DefaultArchetypes),
	}
	if err := team.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	prompt := team.LeaderSystemPrompt()
	for _, want := range []string{
		"worker",
		"reviewer",
		"integrator",
		"leading a small team",
		"Plan first, dispatch second.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("LeaderSystemPrompt missing %q\n--- full ---\n%s", want, prompt)
		}
	}
}

func TestLeaderSystemPrompt_IncludesLeaderStatusGuidance(t *testing.T) {
	team := &Team{
		Name:       "alpha",
		Leader:     LeaderSpec{SystemPrompt: "Ship it."},
		Archetypes: cloneArchetypes(DefaultArchetypes),
	}
	if err := team.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	prompt := team.LeaderSystemPrompt()
	for _, want := range []string{
		"update_leader_status",
		"paragraph",
		"~5",
		"check if the last update_leader_status",
		"turn",
		"non-negotiable",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("LeaderSystemPrompt missing %q\n--- full ---\n%s", want, prompt)
		}
	}
}

// TestLeaderSystemPrompt_IncludesIntegratorWorkflow guards the
// "Integrator workflow" block the leader carries: the contract phrase,
// the fast-forward command, and (because the leader prompt
// interpolates IntegratorForbiddenOps directly) every command name
// from the forbidden-ops list. The latter check is the cheap-but-
// thorough version of Issue 3, option B — extending the constant in
// defaults.go without updating the leader prompt would now fail here.
func TestLeaderSystemPrompt_IncludesIntegratorWorkflow(t *testing.T) {
	team := &Team{
		Name:       "alpha",
		Leader:     LeaderSpec{SystemPrompt: "Ship it."},
		Archetypes: cloneArchetypes(DefaultArchetypes),
	}
	if err := team.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	prompt := team.LeaderSystemPrompt()
	for _, want := range []string{
		"Integrator workflow",
		"merge --ff-only teem/integrator-",
		"git update-ref refs/heads/main",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("LeaderSystemPrompt missing %q\n--- full ---\n%s", want, prompt)
		}
	}
	// Every command name in IntegratorForbiddenOps must show up in the
	// leader prompt — the leader prompt now interpolates the constant
	// directly, so this should always hold. If a future refactor
	// reverts to paraphrasing, this guard catches the drift.
	for _, want := range []string{
		"git update-ref refs/heads/main",
		"git branch -f main",
		"git push -f origin main",
		"git push --force origin main",
		"git push origin HEAD:main",
		"git push origin <sha>:main",
		"git push origin +HEAD:refs/heads/main",
		"git fetch . HEAD:refs/heads/main",
		"git fetch <remote> +<sha>:refs/heads/main",
		"git symbolic-ref HEAD refs/heads/main",
		"git symbolic-ref refs/heads/main",
		"git checkout main --force",
		".git/refs/heads/main",
		"The only ref you may move is refs/heads/teem/integrator-",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("LeaderSystemPrompt missing forbidden-ops entry %q", want)
		}
	}
}

func TestBuildDefaultLeaderPrompt_FoldsClaudeMD(t *testing.T) {
	got := BuildDefaultLeaderPrompt("# alpha\nuse goimports\n")
	if !strings.Contains(got, "leading a small team") {
		t.Errorf("missing default brief")
	}
	if !strings.Contains(got, "--- Project specifics (from CLAUDE.md) ---") {
		t.Errorf("missing project-specifics header")
	}
	if !strings.Contains(got, "use goimports") {
		t.Errorf("missing CLAUDE.md body")
	}

	plain := BuildDefaultLeaderPrompt("")
	if plain != DefaultLeaderBrief {
		t.Errorf("empty claudeMD should return DefaultLeaderBrief verbatim")
	}
}

func TestValidate_Errors(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{
			name: "no archetypes",
			body: `
team:
  name: x
  leader: {system_prompt: p}
`,
			want: "at least one archetype",
		},
		{
			name: "duplicate role",
			body: `
team:
  name: x
  leader: {system_prompt: p}
  archetypes:
    - {role: r, placement: local, max_concurrent: 1}
    - {role: r, placement: local, max_concurrent: 1}
`,
			want: "duplicate role",
		},
		{
			name: "missing max_concurrent",
			body: `
team:
  name: x
  leader: {system_prompt: p}
  archetypes:
    - {role: r, placement: local}
`,
			want: "max_concurrent must be > 0",
		},
		{
			name: "unknown placement",
			body: `
team:
  name: x
  leader: {system_prompt: p}
  archetypes:
    - {role: r, placement: heroku, max_concurrent: 1}
`,
			want: "unknown placement",
		},
		{
			name: "ssh missing working_dir",
			body: `
team:
  name: x
  leader: {system_prompt: p}
  archetypes:
    - {role: r, placement: "ssh:u@h", max_concurrent: 1}
`,
			want: "ssh placement requires working_dir",
		},
		{
			name: "unknown lifecycle",
			body: `
team:
  name: x
  leader: {system_prompt: p}
  archetypes:
    - {role: r, placement: local, max_concurrent: 1, lifecycle: weird}
`,
			want: "unknown lifecycle",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}
