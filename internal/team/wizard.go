package team

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Wizard drives an interactive build of a Team from prompts written to W
// and answers read from R. Splitting prompts off the *os.Stdin/Stdout
// concrete types makes the flow testable.
type Wizard struct {
	R *bufio.Reader
	W io.Writer
}

// NewWizard wires reader/writer.
func NewWizard(r io.Reader, w io.Writer) *Wizard {
	return &Wizard{R: bufio.NewReader(r), W: w}
}

// Run walks the operator through creating a team. Returns the assembled
// Team (validated) and the serialized YAML bytes ready to write to disk.
// Returns ErrCancelled if the operator declines the final write step.
func (z *Wizard) Run() (*Team, []byte, error) {
	z.println("teem init — let's build a team.")
	z.println("Answers in [brackets] are defaults. Hit enter to accept.")
	z.println("")

	name, err := z.askRequired("Team name", "")
	if err != nil {
		return nil, nil, err
	}

	z.println("")
	z.println("Leader system prompt (the brief your Leader Claude works from).")
	z.println("Enter one or more lines; finish with a blank line.")
	prompt, err := z.askMultiline()
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, nil, fmt.Errorf("leader system prompt is required")
	}

	t := &Team{
		Name:   name,
		Leader: LeaderSpec{SystemPrompt: prompt},
	}

	z.println("")
	z.println("Now add team members. Leader is already set; everyone else is optional.")
	z.println("Common roles: worker, reviewer, integrator, backend, frontend.")
	z.println("")

	roleCounts := map[string]int{}
	for {
		add, err := z.askYesNo("Add an agent?", true)
		if err != nil {
			return nil, nil, err
		}
		if !add {
			break
		}
		agent, err := z.askAgent(roleCounts)
		if err != nil {
			return nil, nil, err
		}
		t.Agents = append(t.Agents, agent)
		z.printf("  ✓ added %s (%s)\n\n", agent.ID, agent.Role)
	}

	if err := t.Validate(); err != nil {
		return nil, nil, fmt.Errorf("validate: %w", err)
	}

	// Marshal under the same fileWrapper team.Load expects.
	body, err := yaml.Marshal(fileWrapper{Team: *t})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal: %w", err)
	}

	z.println("")
	z.println("Team summary:")
	z.println(indent(string(body), "  "))

	keep, err := z.askYesNo("Write this team?", true)
	if err != nil {
		return nil, nil, err
	}
	if !keep {
		return nil, nil, ErrCancelled
	}
	return t, body, nil
}

// ErrCancelled is returned when the operator declines the final write.
var ErrCancelled = fmt.Errorf("wizard: cancelled")

func (z *Wizard) askAgent(counts map[string]int) (AgentSpec, error) {
	role, err := z.askRequired("  Role", "worker")
	if err != nil {
		return AgentSpec{}, err
	}
	counts[role]++
	defaultID := fmt.Sprintf("%s-%d", role, counts[role])
	id, err := z.askRequired("  Agent id", defaultID)
	if err != nil {
		return AgentSpec{}, err
	}
	desc, err := z.ask("  Description (optional)", "")
	if err != nil {
		return AgentSpec{}, err
	}

	placement, err := z.askChoice("  Where does this agent run?", []string{
		"local  — same host as the leader, isolated git worktree",
		"ssh    — remote host you reach via SSH",
		"fargate — ephemeral container on AWS ECS Fargate",
	}, 0)
	if err != nil {
		return AgentSpec{}, err
	}

	a := AgentSpec{ID: id, Role: role, Description: desc}
	switch placement {
	case 0:
		a.Local = true
	case 1:
		target, err := z.askRequired("    SSH target (user@host)", "")
		if err != nil {
			return AgentSpec{}, err
		}
		a.SSHTarget = target
		wd, err := z.askRequired("    Working dir on remote host", "/home/"+leftOfAt(target)+"/teem-"+id)
		if err != nil {
			return AgentSpec{}, err
		}
		a.WorkingDir = wd
	case 2:
		a.Backend = "fargate"
	}
	return a, nil
}

func leftOfAt(s string) string {
	if i := strings.Index(s, "@"); i >= 0 {
		return s[:i]
	}
	return s
}

// --- low-level prompt helpers ---

func (z *Wizard) ask(label, def string) (string, error) {
	if def == "" {
		z.printf("%s: ", label)
	} else {
		z.printf("%s [%s]: ", label, def)
	}
	line, err := z.R.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return def, nil
	}
	return line, nil
}

func (z *Wizard) askRequired(label, def string) (string, error) {
	for {
		v, err := z.ask(label, def)
		if err != nil {
			return "", err
		}
		if v != "" {
			return v, nil
		}
		z.println("  (required — please answer)")
	}
}

func (z *Wizard) askYesNo(label string, def bool) (bool, error) {
	yn := "Y/n"
	if !def {
		yn = "y/N"
	}
	for {
		z.printf("%s [%s]: ", label, yn)
		line, err := z.R.ReadString('\n')
		if err != nil && line == "" {
			return false, err
		}
		s := strings.ToLower(strings.TrimSpace(line))
		switch s {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		z.println("  (please answer y or n)")
	}
}

func (z *Wizard) askChoice(label string, options []string, def int) (int, error) {
	z.printf("%s\n", label)
	for i, o := range options {
		z.printf("    %d) %s\n", i+1, o)
	}
	for {
		z.printf("  Choose [1-%d, default %d]: ", len(options), def+1)
		line, err := z.R.ReadString('\n')
		if err != nil && line == "" {
			return 0, err
		}
		s := strings.TrimSpace(line)
		if s == "" {
			return def, nil
		}
		n, err := strconv.Atoi(s)
		if err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		z.printf("  (please enter a number 1-%d)\n", len(options))
	}
}

// askMultiline reads lines until the operator hits a blank line. A
// trailing newline is preserved on the joined result so YAML's literal
// block scalar (|) renders cleanly.
func (z *Wizard) askMultiline() (string, error) {
	var lines []string
	for {
		z.printf("> ")
		line, err := z.R.ReadString('\n')
		if err != nil && line == "" {
			return strings.Join(lines, "\n"), err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func (z *Wizard) println(s string) { fmt.Fprintln(z.W, s) }

func (z *Wizard) printf(format string, args ...any) { fmt.Fprintf(z.W, format, args...) }

func indent(s, prefix string) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, p := range parts {
		parts[i] = prefix + p
	}
	return strings.Join(parts, "\n")
}

// Summary returns a one-screen description of an existing team, used by
// `teem init` when a YAML already exists.
func Summary(t *Team) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Team: %s\n", t.Name)
	first := strings.SplitN(strings.TrimSpace(t.Leader.SystemPrompt), "\n", 2)[0]
	if len(first) > 80 {
		first = first[:77] + "..."
	}
	fmt.Fprintf(&b, "Leader brief: %s\n", first)
	if len(t.Agents) == 0 {
		fmt.Fprintln(&b, "Agents: (none — leader only)")
		return b.String()
	}
	fmt.Fprintln(&b, "Agents:")
	for _, a := range t.Agents {
		placement := "local"
		switch {
		case a.SSHTarget != "":
			placement = "ssh " + a.SSHTarget
		case a.Backend != "":
			placement = a.Backend
		}
		fmt.Fprintf(&b, "  - %s (%s) @ %s — %s\n", a.ID, a.Role, placement, a.Description)
	}
	return b.String()
}
