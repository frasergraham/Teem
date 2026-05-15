package team

import "testing"

func TestPersonaName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"worker-uma", "Coder Uma"},
		{"reviewer-bex", "Reviewer Bex"},
		{"integrator-pax", "Integrator Pax"},
		{"project_manager-una", "PM Una"},
		// Unknown role prefix → unchanged.
		{"leader", "leader"},
		{"scout-jay", "scout-jay"},
		// Malformed inputs → unchanged.
		{"", ""},
		{"worker-", "worker-"},
		{"-uma", "-uma"},
		{"plain", "plain"},
	}
	for _, c := range cases {
		if got := PersonaName(c.in); got != c.want {
			t.Errorf("PersonaName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
