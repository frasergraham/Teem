package team

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type Team struct {
	Name    string      `yaml:"name"`
	Tailnet TailnetSpec `yaml:"tailnet"`
	Leader  LeaderSpec  `yaml:"leader"`
	Agents  []AgentSpec `yaml:"agents"`

	// mu guards concurrent mutation via Add/Remove/Update methods and
	// makes the read-side helpers (FindAgentByRole etc.) safe to call
	// from goroutines that may race with the daemon's MCP tools.
	mu sync.RWMutex `yaml:"-"`
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
	ID          string   `yaml:"id"`
	Role        string   `yaml:"role"`
	Description string   `yaml:"description"`
	SSHTarget   string   `yaml:"ssh_target,omitempty"`
	Local       bool     `yaml:"local,omitempty"`
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
	Lifecycle  string   `yaml:"lifecycle,omitempty"`
	MCPs       []MCPRef `yaml:"mcps,omitempty"`
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
	Name    string      `yaml:"name"`
	Tailnet TailnetSpec `yaml:"tailnet,omitempty"`
	Leader  LeaderSpec  `yaml:"leader"`
	Agents  []AgentSpec `yaml:"agents,omitempty"`
}

// MarshalYAML serializes the team to YAML in the canonical fileWrapper
// shape ("team:" at the top level). The mutex is intentionally not
// part of the on-disk representation.
func (t *Team) MarshalYAML() ([]byte, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return yaml.Marshal(marshalWrapper{Team: marshalShape{
		Name:    t.Name,
		Tailnet: t.Tailnet,
		Leader:  t.Leader,
		Agents:  append([]AgentSpec(nil), t.Agents...),
	}})
}

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
	seen := map[string]struct{}{}
	for i, a := range t.Agents {
		if a.ID == "" {
			return fmt.Errorf("agents[%d]: id is required", i)
		}
		if _, dup := seen[a.ID]; dup {
			return fmt.Errorf("agents[%d]: duplicate id %q", i, a.ID)
		}
		seen[a.ID] = struct{}{}
		if a.Role == "" {
			return fmt.Errorf("agents[%d] (%s): role is required", i, a.ID)
		}
		placements := 0
		if a.Local {
			placements++
		}
		if a.SSHTarget != "" {
			placements++
		}
		if a.Backend != "" {
			placements++
			if _, ok := SupportedBackends[a.Backend]; !ok {
				return fmt.Errorf("agents[%d] (%s): unknown backend %q (supported: fargate)", i, a.ID, a.Backend)
			}
		}
		if placements == 0 {
			return fmt.Errorf("agents[%d] (%s): must set exactly one of local: true, ssh_target, or backend", i, a.ID)
		}
		if placements > 1 {
			return fmt.Errorf("agents[%d] (%s): set exactly one of local, ssh_target, or backend (got %d)", i, a.ID, placements)
		}
		if a.Lifecycle != "" {
			if _, ok := SupportedLifecycles[a.Lifecycle]; !ok {
				return fmt.Errorf("agents[%d] (%s): unknown lifecycle %q (supported: ephemeral, persistent)", i, a.ID, a.Lifecycle)
			}
		}
	}
	return nil
}

// LeaderSystemPrompt builds the system prompt the Leader subprocess is
// launched with: a fixed Teem preamble + the team roster + the YAML's
// leader.system_prompt verbatim.
func (t *Team) LeaderSystemPrompt() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var b strings.Builder
	b.WriteString("You are the Leader of a Teem — a small team of Claude Code agents working together on a software project.\n\n")
	b.WriteString("You have access to MCP tools that let you spawn agents, assign jobs, inspect status, and read the shared message bus. ")
	b.WriteString("Use them to coordinate the team rather than doing every task yourself.\n\n")
	b.WriteString("Your team:\n")
	if len(t.Agents) == 0 {
		b.WriteString("  (no agents declared yet)\n")
	}
	for _, a := range t.Agents {
		fmt.Fprintf(&b, "  - %s (%s): %s\n", a.ID, a.Role, a.Description)
	}
	b.WriteString("\n--- Project brief ---\n")
	b.WriteString(strings.TrimSpace(t.Leader.SystemPrompt))
	b.WriteString("\n")
	return b.String()
}

// FindAgentByRole returns the first agent matching the role, or nil. The
// returned pointer references team-internal storage and is only safe to
// read; mutations should go through AddAgent/RemoveAgent/UpdateAgent.
func (t *Team) FindAgentByRole(role string) *AgentSpec {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for i := range t.Agents {
		if t.Agents[i].Role == role {
			a := t.Agents[i]
			return &a
		}
	}
	return nil
}

// FindAgentByID returns the agent with the given id, or nil. See
// FindAgentByRole for the same caveat about mutation.
func (t *Team) FindAgentByID(id string) *AgentSpec {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for i := range t.Agents {
		if t.Agents[i].ID == id {
			a := t.Agents[i]
			return &a
		}
	}
	return nil
}

// Snapshot returns a copy of the agent roster safe to iterate without
// holding the lock. Used by MCP tools that report the current team
// shape.
func (t *Team) Snapshot() []AgentSpec {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]AgentSpec, len(t.Agents))
	copy(out, t.Agents)
	return out
}

// AddAgent appends spec to the roster after validating it. Returns
// ErrAgentExists if an agent with the same id is already in the
// roster, or a validation error if the spec is malformed.
func (t *Team) AddAgent(spec AgentSpec) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if spec.ID == "" {
		return fmt.Errorf("AddAgent: id is required")
	}
	if spec.Role == "" {
		return fmt.Errorf("AddAgent: role is required")
	}
	for _, a := range t.Agents {
		if a.ID == spec.ID {
			return ErrAgentExists
		}
	}
	if err := validatePlacement(spec); err != nil {
		return err
	}
	if spec.Lifecycle != "" {
		if _, ok := SupportedLifecycles[spec.Lifecycle]; !ok {
			return fmt.Errorf("AddAgent: unknown lifecycle %q", spec.Lifecycle)
		}
	}
	t.Agents = append(t.Agents, spec)
	return nil
}

// RemoveAgent drops the agent with the given id from the roster.
// Returns ErrAgentNotFound when no such agent exists.
func (t *Team) RemoveAgent(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, a := range t.Agents {
		if a.ID == id {
			t.Agents = append(t.Agents[:i], t.Agents[i+1:]...)
			return nil
		}
	}
	return ErrAgentNotFound
}

// UpdateAgentDescription mutates an existing agent's description. Other
// fields are immutable post-creation; to change placement or lifecycle,
// remove and re-add.
func (t *Team) UpdateAgentDescription(id, description string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.Agents {
		if t.Agents[i].ID == id {
			t.Agents[i].Description = description
			return nil
		}
	}
	return ErrAgentNotFound
}

// ErrAgentExists / ErrAgentNotFound are returned by the mutator methods
// so callers can distinguish "no-op" from "real failure".
var (
	ErrAgentExists   = fmt.Errorf("team: agent already exists")
	ErrAgentNotFound = fmt.Errorf("team: agent not found")
)

// validatePlacement enforces the same exactly-one-of rule the YAML
// validator does, plus the supported-backend check.
func validatePlacement(a AgentSpec) error {
	placements := 0
	if a.Local {
		placements++
	}
	if a.SSHTarget != "" {
		placements++
	}
	if a.Backend != "" {
		placements++
		if _, ok := SupportedBackends[a.Backend]; !ok {
			return fmt.Errorf("unknown backend %q (supported: fargate)", a.Backend)
		}
	}
	if placements == 0 {
		return fmt.Errorf("must set exactly one of local, ssh_target, or backend")
	}
	if placements > 1 {
		return fmt.Errorf("set exactly one of local, ssh_target, or backend (got %d)", placements)
	}
	return nil
}
