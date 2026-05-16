package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/frasergraham/teem/internal/plan"
)

// TestRenderTeamIndex_RendersTeamCard locks in the multi-team overview
// at GET /: each registered team renders as a card with a link to its
// SPA, the leader-status preview is surfaced when present, and the
// "Open" counter reflects plan.Task counts.
func TestRenderTeamIndex_RendersTeamCard(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}

	alpha := newFullTestTeam(t, "alpha")
	bravo := newFullTestTeam(t, "bravo")
	d.teams["alpha"] = alpha
	d.teams["bravo"] = bravo

	if _, err := alpha.plan.AddTask(plan.NewTaskInput{Title: "alpha-1"}); err != nil {
		t.Fatalf("alpha AddTask: %v", err)
	}
	if _, err := alpha.plan.AddTask(plan.NewTaskInput{Title: "alpha-2"}); err != nil {
		t.Fatalf("alpha AddTask: %v", err)
	}
	if err := alpha.leaderStatus.Set("leader", "watching alpha integrate", nil); err != nil {
		t.Fatalf("Set leader status: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type=%q want text/html", ct)
	}

	body := w.Body.String()

	// Team names render as links to their SPA path.
	for _, want := range []string{
		`href="/teams/alpha/"`,
		`>alpha<`,
		`href="/teams/bravo/"`,
		`>bravo<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; excerpt:\n%s", want, excerpt(body))
		}
	}

	// Leader status surfaces (text + "Leader" pill not needed; the
	// truncated preview itself is the signal).
	if !strings.Contains(body, "watching alpha integrate") {
		t.Errorf("alpha leader status text missing; excerpt:\n%s", excerpt(body))
	}

	// "No leader status" placeholder fires for bravo (no Set called).
	if !strings.Contains(body, "No leader status yet.") {
		t.Errorf("bravo leader placeholder missing; excerpt:\n%s", excerpt(body))
	}

	// Open count: alpha has 2 open tasks. The counter pairs the
	// numeral with the "Open" label, so assert on a fragment that
	// keeps the two adjacent.
	if !strings.Contains(body, `>2</span><span class="l">Open<`) {
		t.Errorf("alpha open-task counter (2) missing; excerpt:\n%s", excerpt(body))
	}

	// Cards are sorted by Name (alpha before bravo).
	if i, j := strings.Index(body, ">alpha<"), strings.Index(body, ">bravo<"); !(i >= 0 && j > i) {
		t.Errorf("expected alpha card before bravo (i=%d j=%d)", i, j)
	}
}

// TestRenderTeamIndex_EmptyState renders the empty-state copy when no
// teams are registered, preserving the operator's bookmark surface.
func TestRenderTeamIndex_EmptyState(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "No teams registered.") {
		t.Errorf("empty-state copy missing; excerpt:\n%s", excerpt(w.Body.String()))
	}
}
