package main

import (
	"testing"

	"github.com/frasergraham/teem/internal/audit"
)

type recordedPush struct {
	body string
	meta map[string]string
}

func TestChannelHook_FiltersInterestingKinds(t *testing.T) {
	var got []recordedPush
	push := func(body string, meta map[string]string) {
		got = append(got, recordedPush{body: body, meta: meta})
	}
	hook := makeChannelHook(push)

	hook([]audit.Event{
		{
			AgentID: "worker-ada",
			JobID:   "j-1234",
			Kind:    audit.KindJobComplete,
		},
		// Heartbeats are high-volume noise — must NOT push.
		{
			AgentID: "worker-ada",
			Kind:    audit.KindHeartbeat,
		},
		// JobReceived is also out-of-scope; only terminal kinds push.
		{
			AgentID: "worker-ada",
			JobID:   "j-5678",
			Kind:    audit.KindJobReceived,
		},
	})

	if len(got) != 1 {
		t.Fatalf("expected exactly 1 push for job_complete; got %d: %+v", len(got), got)
	}
	if got[0].meta["kind"] != "job_complete" {
		t.Fatalf("meta.kind = %q, want job_complete", got[0].meta["kind"])
	}
	if got[0].meta["agent_id"] != "worker-ada" {
		t.Fatalf("meta.agent_id = %q, want worker-ada", got[0].meta["agent_id"])
	}
	if got[0].meta["job_id"] != "j-1234" {
		t.Fatalf("meta.job_id = %q, want j-1234", got[0].meta["job_id"])
	}
	if got[0].body == "" {
		t.Fatal("empty body for job_complete push")
	}
}

func TestChannelHook_CarriesTaskID(t *testing.T) {
	var got []recordedPush
	hook := makeChannelHook(func(body string, meta map[string]string) {
		got = append(got, recordedPush{body: body, meta: meta})
	})

	hook([]audit.Event{{
		AgentID: "leader",
		Kind:    audit.KindBlockerNote,
		Message: "needs operator action",
		Meta:    map[string]any{"task_id": "t-abcd"},
	}})

	if len(got) != 1 {
		t.Fatalf("expected 1 push; got %d", len(got))
	}
	if got[0].meta["task_id"] != "t-abcd" {
		t.Fatalf("meta.task_id = %q, want t-abcd", got[0].meta["task_id"])
	}
}

func TestIsChannelKind_Matrix(t *testing.T) {
	cases := []struct {
		kind audit.Kind
		want bool
	}{
		{audit.KindJobComplete, true},
		{audit.KindJobError, true},
		{audit.KindJobInterrupted, true},
		{audit.KindWorkerStopped, true},
		{audit.KindBlockerNote, true},
		{audit.KindDecisionNote, true},
		{audit.KindTaskStageChanged, true},
		{audit.KindHeartbeat, false},
		{audit.KindJobReceived, false},
		{audit.KindNote, false},
		{audit.Kind("pulse_tick"), false},
	}
	for _, c := range cases {
		if got := isChannelKind(c.kind); got != c.want {
			t.Errorf("isChannelKind(%q) = %v, want %v", c.kind, got, c.want)
		}
	}
}
