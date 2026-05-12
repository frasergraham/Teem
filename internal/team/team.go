package team

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type Team struct {
	Name    string       `yaml:"name"`
	Tailnet TailnetSpec  `yaml:"tailnet"`
	Leader  LeaderSpec   `yaml:"leader"`
	Agents  []AgentSpec  `yaml:"agents"`
}

type TailnetSpec struct {
	Hostname    string `yaml:"hostname"`
	AuthKeyEnv  string `yaml:"auth_key_env"`
}

type LeaderSpec struct {
	SystemPrompt string   `yaml:"system_prompt"`
	MCPs         []MCPRef `yaml:"mcps"`
}

type AgentSpec struct {
	ID          string   `yaml:"id"`
	Role        string   `yaml:"role"`
	Description string   `yaml:"description"`
	SSHTarget   string   `yaml:"ssh_target,omitempty"`
	Local       bool     `yaml:"local,omitempty"`
	WorkingDir  string   `yaml:"working_dir,omitempty"`
	MCPs        []MCPRef `yaml:"mcps,omitempty"`
}

type MCPRef struct {
	Name    string            `yaml:"name"`
	Command string            `yaml:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty"`
	URL     string            `yaml:"url,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
}

type fileWrapper struct {
	Team Team `yaml:"team"`
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
		if !a.Local && a.SSHTarget == "" {
			return fmt.Errorf("agents[%d] (%s): must set local: true or ssh_target", i, a.ID)
		}
		if a.Local && a.SSHTarget != "" {
			return fmt.Errorf("agents[%d] (%s): cannot be both local and ssh_target", i, a.ID)
		}
	}
	return nil
}

// LeaderSystemPrompt builds the system prompt the Leader subprocess is
// launched with: a fixed Teem preamble + the team roster + the YAML's
// leader.system_prompt verbatim.
func (t *Team) LeaderSystemPrompt() string {
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

// FindAgentByRole returns the first agent matching the role, or nil.
func (t *Team) FindAgentByRole(role string) *AgentSpec {
	for i := range t.Agents {
		if t.Agents[i].Role == role {
			return &t.Agents[i]
		}
	}
	return nil
}

// FindAgentByID returns the agent with the given id, or nil.
func (t *Team) FindAgentByID(id string) *AgentSpec {
	for i := range t.Agents {
		if t.Agents[i].ID == id {
			return &t.Agents[i]
		}
	}
	return nil
}
