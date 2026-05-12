package tailnet

import "testing"

func TestValidHostname(t *testing.T) {
	good := []string{"a", "teem-leader", "x9", "node-1"}
	for _, s := range good {
		if !validHostname(s) {
			t.Errorf("expected %q to be valid", s)
		}
	}
	bad := []string{"", "-teem", "teem-", "Teem!", "with spaces", "a..b"}
	for _, s := range bad {
		if validHostname(s) {
			t.Errorf("expected %q to be invalid", s)
		}
	}
}

func TestNew_RejectsBadHostname(t *testing.T) {
	if _, err := New(Config{Hostname: "Bad Name!"}); err == nil {
		t.Fatal("expected error for invalid hostname")
	}
	if _, err := New(Config{Hostname: ""}); err == nil {
		t.Fatal("expected error for empty hostname")
	}
}
