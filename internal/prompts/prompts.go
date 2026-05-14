// Package prompts assembles the system prompts handed to the leader
// and each archetype worker from layered sources:
//
//  1. The team YAML (leader.system_prompt, archetype.description).
//  2. Operator overrides on disk under
//     ~/.teem/state/<team-slug>/prompt-overrides/<role>.md.
//  3. Standing system blocks the leader/worker always sees (the
//     existing LeaderSystemPrompt preamble, the archetype framing).
//
// Overrides are operator-authored markdown. AppendOverride adds a
// timestamped block so a history of operator tweaks survives in the
// file. Writes are atomic (tmp + rename) so a crash mid-write can't
// truncate the file.
package prompts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/frasergraham/teem/internal/team"
)

// LeaderRole is the synthetic role name the Builder accepts for the
// leader's prompt. It can never collide with an archetype role because
// archetype role validation forbids any of the punctuation we'd need.
const LeaderRole = "leader"

// roleNameRE matches an acceptable role string passed to the Builder.
// Restricted to a-z, 0-9, _ and - and required to start with a
// lowercase letter so a malicious or careless role name can't escape
// the prompt-overrides directory. Kept identical to the archmem
// package's role slug grammar — see internal/archmem.IsValidRoleName.
var roleNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// ErrInvalidRole is returned when role contains characters outside the
// allowed set or is empty.
var ErrInvalidRole = errors.New("prompts: invalid role name")

// Builder assembles prompts for the team's leader and archetypes, with
// an optional override layer on disk.
type Builder struct {
	team        *team.Team
	overrideDir string

	mu      sync.Mutex
	roleMus map[string]*sync.Mutex
}

// New returns a Builder. overrideDir may be "" — in that case the
// builder still works and returns just the YAML-derived prompts. When
// non-empty, the dir is created lazily on first write.
func New(t *team.Team, overrideDir string) *Builder {
	return &Builder{
		team:        t,
		overrideDir: overrideDir,
		roleMus:     map[string]*sync.Mutex{},
	}
}

// ValidateRole reports whether role is safe to use as a path segment.
// Exposed so callers (CLI/MCP handlers) can reject bad input early.
func ValidateRole(role string) error {
	if role == "" {
		return ErrInvalidRole
	}
	if !roleNameRE.MatchString(role) {
		return ErrInvalidRole
	}
	return nil
}

// OverrideDir returns the on-disk directory holding override files.
// May be empty if the Builder was constructed without one.
func (b *Builder) OverrideDir() string { return b.overrideDir }

// OverridePath returns the on-disk path for role's override file. The
// file may not exist; this is the location AppendOverride / the editor
// would write to. Returns "" if overrideDir is unset or role is bad.
func (b *Builder) OverridePath(role string) string {
	if b.overrideDir == "" {
		return ""
	}
	if err := ValidateRole(role); err != nil {
		return ""
	}
	return filepath.Join(b.overrideDir, role+".md")
}

// Override returns the raw override body for role and a found flag.
// A non-existent override file is not an error — (string, false, nil).
func (b *Builder) Override(role string) (string, bool, error) {
	if err := ValidateRole(role); err != nil {
		return "", false, err
	}
	p := b.OverridePath(role)
	if p == "" {
		return "", false, nil
	}
	body, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("prompts: read override %s: %w", p, err)
	}
	return string(body), true, nil
}

// Leader returns the assembled system prompt for the leader: the
// existing Team.LeaderSystemPrompt body followed by the override layer
// (when present).
func (b *Builder) Leader() string {
	base := ""
	if b.team != nil {
		base = b.team.LeaderSystemPrompt()
	}
	return b.combine(base, LeaderRole)
}

// Archetype returns the assembled system prompt for a worker spawning
// under role: a "you are X" framing built from the archetype's YAML
// description, then the override layer. The second return is false
// when role is unknown to the team (no archetype with that role and
// role != LeaderRole) — callers that hit this should treat it as
// "role not declared" rather than silently shipping the empty
// fallback framing, which would otherwise look like a valid prompt.
// Real callers already pre-filter via team.FindArchetypeByRole, but
// the explicit ok lets future callers fail loud.
func (b *Builder) Archetype(role string) (string, bool) {
	if b.team == nil {
		return "", false
	}
	if arch := b.team.FindArchetypeByRole(role); arch == nil {
		return "", false
	}
	base := b.baseArchetype(role)
	return b.combine(base, role), true
}

// baseArchetype renders the YAML-derived archetype framing. Used as
// the (a)-layer in the assembly. Caller must have already verified
// role is known to the team — this just renders the framing from the
// matched archetype, with a minimal fallback if the lookup misses
// (shouldn't happen in production paths).
func (b *Builder) baseArchetype(role string) string {
	var description string
	var lifecycle string
	var placement string
	if b.team != nil {
		if arch := b.team.FindArchetypeByRole(role); arch != nil {
			description = strings.TrimSpace(arch.Description)
			lifecycle = arch.LifecycleOrDefault()
			placement = arch.Placement
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "You are a %s worker on a Teem.\n", role)
	if description != "" {
		fmt.Fprintf(&sb, "\nRole brief: %s\n", description)
	}
	if placement != "" {
		fmt.Fprintf(&sb, "\nPlacement: %s (%s).\n", placement, lifecycle)
	}
	return sb.String()
}

// combine glues the base prompt and the override layer with a clear
// separator. Empty override → base verbatim. Empty base + non-empty
// override → just the override. Trailing whitespace is normalised so
// the output always ends in a single newline.
func (b *Builder) combine(base, role string) string {
	override, _, _ := b.Override(role)
	override = strings.TrimSpace(override)
	base = strings.TrimRight(base, "\n")
	switch {
	case override == "" && base == "":
		return ""
	case override == "":
		return base + "\n"
	case base == "":
		return override + "\n"
	}
	return base + "\n\n--- Operator overrides ---\n" + override + "\n"
}

// AppendOverride atomically appends a timestamped block to the role's
// override file. Creates overrideDir + file on first call. The block
// shape is:
//
//	## Appended <RFC3339-UTC>
//	<text>
//
// Leading + trailing whitespace in text is trimmed; the block is
// always preceded by a blank line.
func (b *Builder) AppendOverride(role, text string) error {
	if err := ValidateRole(role); err != nil {
		return err
	}
	if b.overrideDir == "" {
		return errors.New("prompts: override directory not configured")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("prompts: append text is empty")
	}
	if err := os.MkdirAll(b.overrideDir, 0o700); err != nil {
		return fmt.Errorf("prompts: mkdir: %w", err)
	}

	mu := b.lockRole(role)
	defer mu.Unlock()

	p := b.OverridePath(role)
	existing, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("prompts: read override: %w", err)
	}
	var sb strings.Builder
	sb.Write(existing)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		sb.WriteByte('\n')
	}
	if len(existing) > 0 {
		sb.WriteByte('\n')
	}
	fmt.Fprintf(&sb, "## Appended %s\n%s\n", time.Now().UTC().Format(time.RFC3339), text)

	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o600); err != nil {
		return fmt.Errorf("prompts: write tmp: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("prompts: rename: %w", err)
	}
	return nil
}

func (b *Builder) lockRole(role string) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m, ok := b.roleMus[role]; ok {
		m.Lock()
		return m
	}
	m := &sync.Mutex{}
	b.roleMus[role] = m
	m.Lock()
	return m
}
