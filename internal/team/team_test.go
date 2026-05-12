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
  agents:
    - id: be
      role: backend
      description: "Go services."
      local: true
      working_dir: /tmp/be
    - id: fe
      role: frontend
      description: "React."
      ssh_target: user@frontbox
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
	if len(team.Agents) != 2 {
		t.Fatalf("agents: got %d", len(team.Agents))
	}
	prompt := team.LeaderSystemPrompt()
	for _, want := range []string{"backend", "frontend", "Ship the MVP."} {
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
`)
	team, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if team.Tailnet.Hostname != "big-cats" {
		t.Errorf("hostname: got %q", team.Tailnet.Hostname)
	}
}

func TestValidate_Errors(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{
			name: "duplicate id",
			body: `
team:
  name: x
  leader: {system_prompt: p}
  agents:
    - {id: a, role: r, local: true}
    - {id: a, role: r, local: true}
`,
			want: "duplicate id",
		},
		{
			name: "no placement",
			body: `
team:
  name: x
  leader: {system_prompt: p}
  agents:
    - {id: a, role: r}
`,
			want: "must set exactly one",
		},
		{
			name: "both placements",
			body: `
team:
  name: x
  leader: {system_prompt: p}
  agents:
    - {id: a, role: r, local: true, ssh_target: u@h}
`,
			want: "set exactly one",
		},
		{
			name: "unknown backend",
			body: `
team:
  name: x
  leader: {system_prompt: p}
  agents:
    - {id: a, role: r, backend: heroku}
`,
			want: "unknown backend",
		},
		{
			name: "backend with ssh_target",
			body: `
team:
  name: x
  leader: {system_prompt: p}
  agents:
    - {id: a, role: r, backend: fargate, ssh_target: u@h}
`,
			want: "set exactly one",
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
