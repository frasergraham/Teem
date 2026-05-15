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

// TestLoad_ArchetypeNoWorktreeAndSkill confirms the YAML fields added
// for the project_manager archetype round-trip cleanly: no_worktree
// flips the spawner's worktree-skip path, and skill is forwarded to
// the claude subprocess via --append-system-prompt. Both are
// omitempty so archetypes that don't set them parse exactly as before.
func TestLoad_ArchetypeNoWorktreeAndSkill(t *testing.T) {
	path := writeTemp(t, `
team:
  name: pm
  leader:
    system_prompt: "Ship it."
  archetypes:
    - role: project_manager
      placement: local
      max_concurrent: 1
      no_worktree: true
      skill: linear
    - role: worker
      placement: local
      max_concurrent: 2
`)
	tm, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pm := tm.FindArchetypeByRole("project_manager")
	if pm == nil {
		t.Fatalf("project_manager archetype missing")
	}
	if !pm.NoWorktree {
		t.Errorf("project_manager NoWorktree = false, want true")
	}
	if pm.Skill != "linear" {
		t.Errorf("project_manager Skill = %q, want \"linear\"", pm.Skill)
	}
	// Existing archetypes without the new keys must default to
	// false / empty so they're unaffected.
	w := tm.FindArchetypeByRole("worker")
	if w == nil {
		t.Fatalf("worker archetype missing")
	}
	if w.NoWorktree {
		t.Errorf("worker NoWorktree = true, want false (unset)")
	}
	if w.Skill != "" {
		t.Errorf("worker Skill = %q, want empty", w.Skill)
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

func TestLeaderSystemPrompt_IncludesMemoryHygiene(t *testing.T) {
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
		"Memory hygiene",
		"stage=verified",
		`append_archetype_memory(role="leader"`,
		"<task-id> <title>",
		"learnings:",
		"Do NOT append",
		"t-411da8cc",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("LeaderSystemPrompt missing %q\n--- full ---\n%s", want, prompt)
		}
	}
}

// TestLeaderSystemPrompt_IncludesPMSection asserts the "Working with
// the project manager" block is present whether or not a
// project_manager archetype is in the roster — single source of truth,
// no per-team branching.
func TestLeaderSystemPrompt_IncludesPMSection(t *testing.T) {
	cases := []struct {
		name       string
		archetypes []ArchetypeSpec
	}{
		{
			name:       "without project_manager",
			archetypes: cloneArchetypes(DefaultArchetypes),
		},
		{
			name: "with project_manager",
			archetypes: append(cloneArchetypes(DefaultArchetypes), ArchetypeSpec{
				Role:          "project_manager",
				Description:   "Consults on priorities and tracker.",
				Placement:     "local",
				MaxConcurrent: 1,
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			team := &Team{
				Name:       "alpha",
				Leader:     LeaderSpec{SystemPrompt: "Ship it."},
				Archetypes: tc.archetypes,
			}
			if err := team.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			prompt := team.LeaderSystemPrompt()
			for _, want := range []string{
				"Working with the project manager",
				"project_manager",
				"consult",
				"no rate limit",
				"add_task",
			} {
				if !strings.Contains(prompt, want) {
					t.Errorf("LeaderSystemPrompt missing %q\n--- full ---\n%s", want, prompt)
				}
			}
		})
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

// TestLoad_DoesNotMutateYAMLWithoutID is the regression test for the
// pure-read Load contract. Before this fix, Load auto-minted+wrote a
// `team_id` back into the operator's teem.yaml the first time a CLI
// read-side command (`teem agent show --prompt`, `teem agent list`)
// touched the file. That silently mutated the operator's hand-edited
// YAML during a read — surprising and a bug. Load must now return
// whatever's on disk, byte-for-byte unchanged.
func TestLoad_DoesNotMutateYAMLWithoutID(t *testing.T) {
	body := `# Hand-edited config — comments must survive.
team:
  name: alpha
  leader:
    system_prompt: "Ship the MVP."
  archetypes:
    - role: worker
      description: "Implements features."
      placement: local
      max_concurrent: 1
`
	path := writeTemp(t, body)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	beforeStat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}
	beforeMtime := beforeStat.ModTime()

	tm, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tm.ID != "" {
		t.Errorf("Load returned t.ID=%q on a yaml without id; pure-read Load must surface the disk state, not mint", tm.ID)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("Load mutated the YAML.\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
	afterStat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !afterStat.ModTime().Equal(beforeMtime) {
		t.Errorf("Load touched the file's mtime (before=%v after=%v); pure-read must not write", beforeMtime, afterStat.ModTime())
	}
}

// TestNewID_FormatAndUniqueness exercises the id minter directly: every
// minted id must match the canonical regex (`t-` + 16 lowercase hex
// chars) and ten draws in a row must all differ. A regression that
// e.g. dropped the prefix or shortened the hex would surface here.
func TestNewID_FormatAndUniqueness(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 10; i++ {
		id := NewID()
		if !IsCanonicalID(id) {
			t.Errorf("NewID()[%d] = %q, not canonical", i, id)
		}
		if len(id) != len(IDPrefix)+16 {
			t.Errorf("NewID()[%d] = %q, want length %d", i, id, len(IDPrefix)+16)
		}
		if _, dup := seen[id]; dup {
			t.Errorf("NewID()[%d] = %q, duplicate within 10 draws", i, id)
		}
		seen[id] = struct{}{}
	}
}

// TestIsCanonicalID exercises the regex gate that keeps migration code
// from re-minting an already-minted dir and gates filesystem path
// safety. Both valid and invalid inputs are checked because tightening
// the regex (e.g. requiring uppercase, or relaxing length) silently
// breaks one path or the other.
func TestIsCanonicalID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"t-deadbeef00112233", true},
		{"t-abcdef0123456789", true},
		{"t-12345678", true}, // 8 hex is the regex minimum
		{"", false},
		{"alpha", false},
		{"t-", false},
		{"T-DEADBEEF00112233", false}, // uppercase not allowed
		{"t-DEADBEEF00112233", false}, // mixed case not allowed
		{"t-1234567", false},          // 7 hex below minimum
		{"t-zzzzzzzz", false},         // not hex
		{"alpha-team", false},
		{"t-deadbeef00112233-extra", false},
	}
	for _, c := range cases {
		if got := IsCanonicalID(c.in); got != c.want {
			t.Errorf("IsCanonicalID(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestEnsureIDFile_PreservesComments writes a teem.yaml with comments
// + a non-id `team:` mapping, calls EnsureIDFile, then asserts the
// returned id is canonical AND the file still carries the original
// comments and key order. The point of the yaml.v3 Node API in
// EnsureIDFile is exactly this — a naive yaml.Marshal would strip
// comments and reorder keys, silently destroying the operator's
// hand-edited file.
func TestEnsureIDFile_PreservesComments(t *testing.T) {
	body := `# This is the team manifest.
team:
  # The display name is shown in the dashboard.
  name: alpha
  # Leader is the orchestrator.
  leader:
    system_prompt: "Ship the MVP."
  archetypes:
    - role: worker
      description: "Implements features."
      placement: local
      max_concurrent: 1
`
	path := writeTemp(t, body)
	id, err := EnsureIDFile(path)
	if err != nil {
		t.Fatalf("EnsureIDFile: %v", err)
	}
	if !IsCanonicalID(id) {
		t.Errorf("returned id %q not canonical", id)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	gotStr := string(got)
	for _, want := range []string{
		"# This is the team manifest.",
		"# The display name is shown in the dashboard.",
		"# Leader is the orchestrator.",
		"id: " + id,
		"name: alpha",
	} {
		if !strings.Contains(gotStr, want) {
			t.Errorf("rewritten yaml missing %q\n--- got ---\n%s", want, gotStr)
		}
	}
	// Key order: id should come before name (we prepend it), and name
	// should come before leader. Anything else means yaml.Marshal
	// reordered the file.
	idIdx := strings.Index(gotStr, "id: ")
	nameIdx := strings.Index(gotStr, "name: ")
	leaderIdx := strings.Index(gotStr, "leader:")
	if !(idIdx >= 0 && idIdx < nameIdx && nameIdx < leaderIdx) {
		t.Errorf("key order disturbed: id@%d name@%d leader@%d\n%s", idIdx, nameIdx, leaderIdx, gotStr)
	}
}

// TestEnsureIDFile_RespectsExistingID confirms idempotency: a yaml
// that already has `id:` keeps its id. A regression that re-minted
// every time would orphan the on-disk state dir keyed by the original
// id.
func TestEnsureIDFile_RespectsExistingID(t *testing.T) {
	existing := "t-cafebabedeadbeef"
	body := `team:
  id: ` + existing + `
  name: alpha
  leader: {system_prompt: p}
  archetypes:
    - {role: w, placement: local, max_concurrent: 1}
`
	path := writeTemp(t, body)
	got, err := EnsureIDFile(path)
	if err != nil {
		t.Fatalf("EnsureIDFile: %v", err)
	}
	if got != existing {
		t.Errorf("EnsureIDFile = %q, want existing %q (regression: re-mint)", got, existing)
	}
	// Second call must also be stable.
	got2, err := EnsureIDFile(path)
	if err != nil {
		t.Fatalf("EnsureIDFile (2nd): %v", err)
	}
	if got2 != existing {
		t.Errorf("EnsureIDFile (2nd) = %q, want %q", got2, existing)
	}
}

// TestSetIDFile_AtomicWrite verifies SetIDFile writes the id back and
// preserves file mode. Atomic-write (tmp + rename) means we never see
// a half-written file mid-write; the visible-mode check confirms the
// rename doesn't accidentally reset perms to 0644.
func TestSetIDFile_AtomicWrite(t *testing.T) {
	body := `team:
  name: alpha
  leader: {system_prompt: p}
  archetypes:
    - {role: w, placement: local, max_concurrent: 1}
`
	path := writeTemp(t, body)
	// writeTemp leaves the file at 0o600; capture the original mode.
	beforeStat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	beforeMode := beforeStat.Mode().Perm()

	newID := "t-1122334455667788"
	if err := SetIDFile(path, newID); err != nil {
		t.Fatalf("SetIDFile: %v", err)
	}

	afterBody, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(afterBody), "id: "+newID) {
		t.Errorf("file missing new id:\n%s", afterBody)
	}
	afterStat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if afterStat.Mode().Perm() != beforeMode {
		t.Errorf("perms changed: before=%v after=%v", beforeMode, afterStat.Mode().Perm())
	}

	// SetIDFile must OVERWRITE an existing id (unlike EnsureIDFile,
	// which preserves it). This is what the daemon's migration relies
	// on when back-filling a minted id into the operator's yaml.
	newer := "t-aabbccddeeff0011"
	if err := SetIDFile(path, newer); err != nil {
		t.Fatalf("SetIDFile overwrite: %v", err)
	}
	afterBody, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back 2: %v", err)
	}
	if !strings.Contains(string(afterBody), "id: "+newer) {
		t.Errorf("overwrite missing new id:\n%s", afterBody)
	}
	if strings.Contains(string(afterBody), "id: "+newID) {
		t.Errorf("overwrite left old id behind:\n%s", afterBody)
	}
}
