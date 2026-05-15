package team

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// IDPrefix is prepended to the random hex when minting a team_id.
const IDPrefix = "t-"

// idRE accepts canonical team ids — `t-` followed by 8+ lowercase hex
// chars. Used to keep migration code from re-minting an already-minted
// directory and to gate filesystem path safety.
var idRE = regexp.MustCompile(`^t-[a-f0-9]{8,}$`)

// IsCanonicalID reports whether s looks like a generated team id.
func IsCanonicalID(s string) bool { return idRE.MatchString(s) }

// NewID mints a fresh team id (`t-` + 16 hex chars / 8 random bytes).
// Used by `teem init` to seed the YAML and by the daemon's startup
// migration to back-fill ids onto pre-T33 state dirs.
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("team: read random: " + err.Error())
	}
	return IDPrefix + hex.EncodeToString(b[:])
}

type Team struct {
	// ID is the stable filesystem / routing key for the team. Renaming
	// `name` is now free; ID is what every per-team path is built from.
	// Auto-minted by `teem init` (and back-filled by Load on first read
	// of a pre-T33 yaml).
	ID         string          `yaml:"id,omitempty"`
	Name       string          `yaml:"name"`
	Tailnet    TailnetSpec     `yaml:"tailnet,omitempty"`
	Leader     LeaderSpec      `yaml:"leader"`
	Archetypes []ArchetypeSpec `yaml:"archetypes"`

	// mu guards concurrent mutation via Add/Remove/Update methods and
	// makes the read-side helpers (FindArchetypeByRole etc.) safe to
	// call from goroutines that may race with the daemon's MCP tools.
	mu sync.RWMutex `yaml:"-"`
}

// ArchetypeSpec is a template for spawning worker instances of a given
// role. The leader decides how many to spawn, up to MaxConcurrent.
// Auto-generated instance IDs are `<role>-<name>` where <name> comes
// from the wordlist allocator (internal/roster). Names persist across
// the worker's lifetime and are returned to the pool on stop.
type ArchetypeSpec struct {
	Role        string `yaml:"role"`
	Description string `yaml:"description,omitempty"`
	// Placement is one of: "local", "fargate", "ssh:user@host".
	Placement     string `yaml:"placement"`
	WorkingDir    string `yaml:"working_dir,omitempty"`
	MaxConcurrent int    `yaml:"max_concurrent"`
	// Lifecycle is "ephemeral" (default) or "persistent". Persistent
	// archetypes survive a daemon restart: instances are reconciled
	// from probing teem-<role>-1..N on the tailnet. Persistent + local
	// requires the operator to run `teem-worker` themselves at the
	// matching hostnames.
	Lifecycle string   `yaml:"lifecycle,omitempty"`
	MCPs      []MCPRef `yaml:"mcps,omitempty"`
	// NoWorktree, when true, suppresses the spawner's per-agent git
	// worktree + branch creation. Used by archetypes (e.g.
	// project_manager) whose work lives outside the codebase. The
	// worker still gets a working directory — either the explicit
	// WorkingDir, or the leader's repo root, or os.TempDir() as a
	// fallback.
	NoWorktree bool `yaml:"no_worktree,omitempty"`
	// Skill names a Claude Code skill the worker should load on each
	// job. Empty means "no skill". The spawner injects a brief
	// "invoke /<skill> via the Skill tool" instruction into the
	// claude subprocess's system prompt via --append-system-prompt
	// (there is no dedicated --load-skill CLI flag upstream).
	Skill string `yaml:"skill,omitempty"`
}

// LifecycleOrDefault returns the archetype's lifecycle, defaulting to
// "ephemeral".
func (a ArchetypeSpec) LifecycleOrDefault() string {
	if a.Lifecycle == "" {
		return "ephemeral"
	}
	return a.Lifecycle
}

type TailnetSpec struct {
	Hostname   string `yaml:"hostname,omitempty"`
	AuthKeyEnv string `yaml:"auth_key_env,omitempty"`
}

type LeaderSpec struct {
	SystemPrompt string   `yaml:"system_prompt"`
	MCPs         []MCPRef `yaml:"mcps,omitempty"`
}

type AgentSpec struct {
	ID          string `yaml:"id"`
	Role        string `yaml:"role"`
	Description string `yaml:"description"`
	SSHTarget   string `yaml:"ssh_target,omitempty"`
	Local       bool   `yaml:"local,omitempty"`
	// Backend names a cloud placement strategy (currently "fargate"). Mutually
	// exclusive with Local and SSHTarget. WorkingDir is ignored for cloud
	// backends.
	Backend string `yaml:"backend,omitempty"`
	// WorkingDir is an optional path override.
	//   - Local agents: when unset, Teem creates an isolated git worktree
	//     at ~/.teem/worktrees/<team>/<agent-id> on branch teem/<agent-id>.
	//     When set, the agent runs in this path raw (no worktree).
	//   - SSH agents: required; the directory on the remote host.
	//   - Cloud agents: ignored.
	WorkingDir string `yaml:"working_dir,omitempty"`
	// Lifecycle is "ephemeral" (default) or "persistent". Persistent
	// agents survive a `teem chat` shutdown: their underlying placement
	// is not torn down, and Teem reconciles + reuses on next startup.
	// Persistent local agents require the operator to run `teem-worker`
	// themselves at hostname teem-<id>; persistent cloud agents are
	// launched the first time and reused thereafter.
	Lifecycle string   `yaml:"lifecycle,omitempty"`
	MCPs      []MCPRef `yaml:"mcps,omitempty"`
	// NoWorktree mirrors the archetype field. Propagated by the
	// spawner so the provisioner can skip git worktree creation for
	// this agent.
	NoWorktree bool `yaml:"no_worktree,omitempty"`
	// Skill mirrors the archetype field. Forwarded to the worker so
	// the claude subprocess can be told to invoke the named skill.
	Skill string `yaml:"skill,omitempty"`
}

// SupportedBackends is the set of cloud backend strings accepted in
// agent.backend. Local/ssh placements use the dedicated fields, not
// backend.
var SupportedBackends = map[string]struct{}{
	"fargate": {},
}

// SupportedLifecycles is the set of lifecycle strings accepted in
// agent.lifecycle. Empty string is treated as "ephemeral".
var SupportedLifecycles = map[string]struct{}{
	"ephemeral":  {},
	"persistent": {},
}

// LifecycleOrDefault returns the agent's lifecycle, defaulting to
// "ephemeral" when unset.
func (a AgentSpec) LifecycleOrDefault() string {
	if a.Lifecycle == "" {
		return "ephemeral"
	}
	return a.Lifecycle
}

type MCPRef struct {
	Name    string            `yaml:"name"`
	Command string            `yaml:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty"`
	URL     string            `yaml:"url,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
}

// fileWrapper / marshalShape are the shape of the on-disk YAML.
// marshalShape is a lock-free mirror of Team so we can serialize without
// copying the embedded mutex (which `go vet` rightly flags).
type fileWrapper struct {
	Team Team `yaml:"team"`
}

type marshalWrapper struct {
	Team marshalShape `yaml:"team"`
}

type marshalShape struct {
	ID         string          `yaml:"id,omitempty"`
	Name       string          `yaml:"name"`
	Tailnet    TailnetSpec     `yaml:"tailnet,omitempty"`
	Leader     LeaderSpec      `yaml:"leader"`
	Archetypes []ArchetypeSpec `yaml:"archetypes"`
}

// MarshalYAML serializes the team to YAML in the canonical fileWrapper
// shape ("team:" at the top level). The mutex is intentionally not
// part of the on-disk representation.
func (t *Team) MarshalYAML() ([]byte, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return yaml.Marshal(marshalWrapper{Team: marshalShape{
		ID:         t.ID,
		Name:       t.Name,
		Tailnet:    t.Tailnet,
		Leader:     t.Leader,
		Archetypes: append([]ArchetypeSpec(nil), t.Archetypes...),
	}})
}

// Load parses the team YAML at path and returns the in-memory Team.
//
// Load is pure-read: it never writes to path. If the YAML lacks an
// `id:` key, the returned Team's ID field is empty. Callers that need
// a persisted id (the daemon's register / restore flows; `teem chat`
// in the operator's working tree) must call EnsureIDFile explicitly
// after Load. Read-side CLI commands (`teem agent show/list`,
// `teem audit`, `teem pulse status`, ...) MUST NOT call EnsureIDFile
// — minting from a read path silently rewrites the operator's
// hand-edited teem.yaml the first time they peek at a prompt.
func Load(path string) (*Team, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("team: read %s: %w", path, err)
	}
	var w fileWrapper
	if err := yaml.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("team: parse %s: %w", path, err)
	}
	t := &w.Team
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("team: validate %s: %w", path, err)
	}
	if t.Tailnet.Hostname == "" {
		t.Tailnet.Hostname = sanitizeHostname(t.Name)
	}
	return t, nil
}

// SetIDFile writes the given id into the YAML's top-level `team:`
// mapping. If an `id:` key already exists with a different value, it
// is overwritten. Same comment-preserving Node API as EnsureIDFile.
// Used by the daemon's pre-T33 migration to back-fill the id it just
// minted into the operator's teem.yaml so future Loads pick it up.
func SetIDFile(path, id string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(body, &root); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return fmt.Errorf("unexpected yaml root kind=%d", root.Kind)
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return fmt.Errorf("yaml top is not a mapping (kind=%d)", top.Kind)
	}
	var teamMap *yaml.Node
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "team" {
			teamMap = top.Content[i+1]
			break
		}
	}
	if teamMap == nil || teamMap.Kind != yaml.MappingNode {
		return fmt.Errorf("yaml missing top-level 'team:' mapping")
	}
	for i := 0; i+1 < len(teamMap.Content); i += 2 {
		if teamMap.Content[i].Value == "id" {
			teamMap.Content[i+1].Value = id
			return writeYAMLNode(path, &root)
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "id"}
	valNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: id}
	teamMap.Content = append([]*yaml.Node{keyNode, valNode}, teamMap.Content...)
	return writeYAMLNode(path, &root)
}

// EnsureIDFile reads the team YAML at path; if it lacks an `id:` key
// under the top-level `team:` mapping, mints one and writes the file
// back. Returns the existing or newly-minted id.
//
// Implementation uses yaml.v3's Node API so comments, key order, and
// formatting in the operator's hand-edited YAML survive the rewrite.
// The write is atomic (temp + rename) so a crash mid-write can't leave
// a half-written teem.yaml.
func EnsureIDFile(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(body, &root); err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return "", fmt.Errorf("unexpected yaml root kind=%d", root.Kind)
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return "", fmt.Errorf("yaml top is not a mapping (kind=%d)", top.Kind)
	}
	var teamMap *yaml.Node
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "team" {
			teamMap = top.Content[i+1]
			break
		}
	}
	if teamMap == nil || teamMap.Kind != yaml.MappingNode {
		return "", fmt.Errorf("yaml missing top-level 'team:' mapping")
	}
	for i := 0; i+1 < len(teamMap.Content); i += 2 {
		if teamMap.Content[i].Value == "id" {
			val := teamMap.Content[i+1].Value
			if val == "" {
				// id key present but empty — back-fill in place.
				id := NewID()
				teamMap.Content[i+1].Value = id
				if err := writeYAMLNode(path, &root); err != nil {
					return "", err
				}
				return id, nil
			}
			return val, nil
		}
	}
	id := NewID()
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "id"}
	valNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: id}
	teamMap.Content = append([]*yaml.Node{keyNode, valNode}, teamMap.Content...)
	if err := writeYAMLNode(path, &root); err != nil {
		return "", err
	}
	return id, nil
}

func writeYAMLNode(path string, root *yaml.Node) error {
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	// Preserve the existing file's mode. The operator's teem.yaml is
	// typically 0o600; rewriting at 0o644 would silently downgrade it.
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out.Bytes(), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var hostnameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func sanitizeHostname(name string) string {
	s := strings.ToLower(name)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '_' || r == '-':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "teem-leader"
	}
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}

func (t *Team) Validate() error {
	if t.Name == "" {
		return fmt.Errorf("name is required")
	}
	if t.Leader.SystemPrompt == "" {
		return fmt.Errorf("leader.system_prompt is required")
	}
	if t.Tailnet.Hostname != "" && !hostnameRE.MatchString(t.Tailnet.Hostname) {
		return fmt.Errorf("tailnet.hostname %q is not a valid DNS label", t.Tailnet.Hostname)
	}
	if len(t.Archetypes) == 0 {
		return fmt.Errorf("at least one archetype is required")
	}
	roles := map[string]struct{}{}
	for i, a := range t.Archetypes {
		if a.Role == "" {
			return fmt.Errorf("archetypes[%d]: role is required", i)
		}
		if _, dup := roles[a.Role]; dup {
			return fmt.Errorf("archetypes[%d]: duplicate role %q", i, a.Role)
		}
		roles[a.Role] = struct{}{}
		if a.MaxConcurrent <= 0 {
			return fmt.Errorf("archetypes[%d] (%s): max_concurrent must be > 0", i, a.Role)
		}
		if err := validateArchetypePlacement(a); err != nil {
			return fmt.Errorf("archetypes[%d] (%s): %w", i, a.Role, err)
		}
		if a.Lifecycle != "" {
			if _, ok := SupportedLifecycles[a.Lifecycle]; !ok {
				return fmt.Errorf("archetypes[%d] (%s): unknown lifecycle %q (supported: ephemeral, persistent)", i, a.Role, a.Lifecycle)
			}
		}
	}
	return nil
}

// LeaderSystemPrompt builds the system prompt the Leader subprocess is
// launched with: a fixed Teem preamble + the team's archetypes + the
// YAML's leader.system_prompt verbatim.
func (t *Team) LeaderSystemPrompt() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var b strings.Builder
	b.WriteString("You are the Leader of a Teem — a team of Claude Code workers you spawn and dispatch jobs to.\n\n")
	b.WriteString("You have MCP tools to spawn workers, assign jobs, inspect status, and recall past work. ")
	b.WriteString("Use them to delegate; don't do everything yourself.\n\n")
	b.WriteString("Worker archetypes available (templates — spawn as many as you need up to the cap):\n")
	if len(t.Archetypes) == 0 {
		b.WriteString("  (none declared)\n")
	}
	for _, a := range t.Archetypes {
		lc := a.LifecycleOrDefault()
		fmt.Fprintf(&b, "  - %s (up to %d, %s, %s): %s\n", a.Role, a.MaxConcurrent, a.Placement, lc, a.Description)
	}
	b.WriteString("\nWhen you spawn from an archetype you get an instance id with a wordlist name (e.g. worker-ada, reviewer-blake). Names persist across the worker's lifetime; once retired they return to the pool and may be reincarnated when the wordlist runs out of fresh entries.\n")
	// NOTE: keep in sync with cmd/teem/plugin/skills/teem-orchestration/SKILL.md
	// "Keeping the dashboard honest" section.
	b.WriteString("\n--- Keeping the dashboard honest ---\n")
	b.WriteString("First thing every new turn: check if the last update_leader_status was more than ~5 minutes ago (use get_leader_status). If yes, refresh it BEFORE anything else when responding. This is non-negotiable — the operator watches this panel and stale status erodes their trust in the team.\n")
	b.WriteString("The status itself is a paragraph (2-4 sentences): what's currently in flight, what just landed or completed, what's blocked or waiting, your next planned action. Skip planning rationale beyond that — record_decision is the place for it.\n")
	b.WriteString("Also refresh mid-turn whenever the situation meaningfully changes — a worker finishes, a task moves stage, a blocker is hit. Multiple updates per turn are fine; stale ones are not.\n")
	// NOTE: keep in sync with cmd/teem/plugin/skills/teem-orchestration/SKILL.md
	// "Integrator workflow" section.
	b.WriteString("\n--- Integrator workflow ---\n")
	b.WriteString("When briefing an integrator, instruct them to commit ONLY to their own teem/integrator-<name> branch — never to touch main. After they report done, YOU fast-forward main from the operator's primary worktree: `git merge --ff-only teem/integrator-<name>`. If that fast-forward fails, something diverged — investigate, never force.\n\n")
	b.WriteString("The forbidden-ops list every integrator carries (workers, including the integrator, must NEVER run these):\n\n")
	b.WriteString(IntegratorForbiddenOps)
	b.WriteString("\n")
	// NOTE: keep in sync with cmd/teem/plugin/skills/teem-orchestration/SKILL.md
	// "Working with the project manager" section.
	b.WriteString("\n--- Working with the project manager ---\n")
	b.WriteString("If a project_manager archetype is in the roster, treat it as a consultant — not a subordinate. Spawn one at the START of a major piece of work to confirm priorities, release fit, and the external tracker's view of the backlog. Spawn one again at the END to push completed-work summaries into the tracker.\n\n")
	b.WriteString("There's no rate limit on PM consultations — use it freely whenever you want a sequencing/tracker check.\n\n")
	b.WriteString("The daemon also ticks the project manager on a schedule, so tracker-side work may show up as add_task entries you didn't request.\n\n")
	b.WriteString("The project manager does not assign jobs, move tasks, or make stage decisions — those remain yours.\n")
	// NOTE: keep in sync with cmd/teem/plugin/skills/teem-orchestration/SKILL.md
	// "Memory hygiene" section.
	b.WriteString("\n--- Memory hygiene ---\n")
	b.WriteString("After moving a task to stage=verified, append a single short entry to your own memory via `mcp__teem__append_archetype_memory(role=\"leader\", note=...)`. Keep it under 200 chars. Format:\n\n")
	b.WriteString("  <task-id> <title>: <one-line outcome>. learnings: <one phrase or \"none\">.\n\n")
	b.WriteString("Examples:\n")
	b.WriteString("  t-411da8cc integrator guardrails: forbidden-ops list + leader does ff-merge. learnings: bypass refspecs (HEAD:main, +-prefix) need explicit listing.\n")
	b.WriteString("  t-1664d413 branch cleanup: teem prune-branches + auto on retire + 12h sweep. learnings: live-vs-merged precedence is the only safety case that matters.\n")
	b.WriteString("  t-7d7f0876 agent CLI: unified teem agent {list,show,update}. learnings: shlex-split $EDITOR; raw memory write needs header validation.\n\n")
	b.WriteString("Do NOT append:\n")
	b.WriteString("  - During-progress notes (use task notes or record_decision for those)\n")
	b.WriteString("  - Things already obvious from `git log` or the task title alone\n")
	b.WriteString("  - Praise / completion ceremony — keep it factual\n\n")
	b.WriteString("If a task fails or is abandoned, optionally append a one-liner with the reason if there's a lasting learning (\"X approach doesn't work because Y\").\n\n")
	b.WriteString("The goal: a leader starting cold on this project (new session, no harness memory) reads the folded leader-memory section of its brief and knows the current state without re-reading git history.\n")
	b.WriteString("\n--- Project brief ---\n")
	b.WriteString(strings.TrimSpace(t.Leader.SystemPrompt))
	b.WriteString("\n")
	return b.String()
}

// ErrAgentNotFound is returned when an MCP tool refers to an instance
// id the spawner doesn't know about.
var ErrAgentNotFound = fmt.Errorf("team: agent not found")

// FindArchetypeByRole returns the archetype with the given role, or
// nil. Returned pointer is a copy safe to read; mutations should go
// through Add/Remove/UpdateArchetype.
func (t *Team) FindArchetypeByRole(role string) *ArchetypeSpec {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for i := range t.Archetypes {
		if t.Archetypes[i].Role == role {
			a := t.Archetypes[i]
			return &a
		}
	}
	return nil
}

// SnapshotArchetypes returns a copy of the archetype list safe to
// iterate without holding the lock.
func (t *Team) SnapshotArchetypes() []ArchetypeSpec {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]ArchetypeSpec, len(t.Archetypes))
	copy(out, t.Archetypes)
	return out
}

// AddArchetype appends a new archetype after validating it.
func (t *Team) AddArchetype(spec ArchetypeSpec) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if spec.Role == "" {
		return fmt.Errorf("AddArchetype: role is required")
	}
	for _, a := range t.Archetypes {
		if a.Role == spec.Role {
			return ErrArchetypeExists
		}
	}
	if spec.MaxConcurrent <= 0 {
		return fmt.Errorf("AddArchetype: max_concurrent must be > 0")
	}
	if err := validateArchetypePlacement(spec); err != nil {
		return err
	}
	t.Archetypes = append(t.Archetypes, spec)
	return nil
}

// RemoveArchetype drops the archetype with the given role.
func (t *Team) RemoveArchetype(role string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, a := range t.Archetypes {
		if a.Role == role {
			t.Archetypes = append(t.Archetypes[:i], t.Archetypes[i+1:]...)
			return nil
		}
	}
	return ErrArchetypeNotFound
}

// UpdateArchetypeDescription replaces the description text on an
// existing archetype.
func (t *Team) UpdateArchetypeDescription(role, description string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.Archetypes {
		if t.Archetypes[i].Role == role {
			t.Archetypes[i].Description = description
			return nil
		}
	}
	return ErrArchetypeNotFound
}

// SetArchetypeMaxConcurrent updates the cap on concurrent instances of
// the role.
func (t *Team) SetArchetypeMaxConcurrent(role string, max int) error {
	if max <= 0 {
		return fmt.Errorf("max_concurrent must be > 0")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.Archetypes {
		if t.Archetypes[i].Role == role {
			t.Archetypes[i].MaxConcurrent = max
			return nil
		}
	}
	return ErrArchetypeNotFound
}

// ErrArchetypeExists / ErrArchetypeNotFound mirror the agent variants.
var (
	ErrArchetypeExists   = fmt.Errorf("team: archetype already exists")
	ErrArchetypeNotFound = fmt.Errorf("team: archetype not found")
)

// validateArchetypePlacement enforces the placement string format and
// supported-backend constraints. Placements: "local",
// "ssh:user@host", or "fargate".
func validateArchetypePlacement(a ArchetypeSpec) error {
	switch {
	case a.Placement == "local":
		return nil
	case a.Placement == "fargate":
		return nil
	case strings.HasPrefix(a.Placement, "ssh:"):
		if strings.TrimPrefix(a.Placement, "ssh:") == "" {
			return fmt.Errorf("ssh placement requires a target (e.g. ssh:user@host)")
		}
		if a.WorkingDir == "" {
			return fmt.Errorf("ssh placement requires working_dir")
		}
		return nil
	case a.Placement == "":
		return fmt.Errorf("placement is required")
	default:
		return fmt.Errorf("unknown placement %q (supported: local, ssh:user@host, fargate)", a.Placement)
	}
}
