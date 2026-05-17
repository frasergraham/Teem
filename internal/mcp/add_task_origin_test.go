package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/plan"
)

// TestAddTask_DefaultsOriginByCallerRole exercises the caller-role
// fallback: a leader-caller produces OriginLeader, a project_manager-*
// caller produces OriginProjectManager, and the default (no agent_id)
// also lands at OriginLeader because the leader is the dominant caller.
func TestAddTask_DefaultsOriginByCallerRole(t *testing.T) {
	srv, _, _, _ := newTestServerFull(t)
	cases := []struct {
		name    string
		agentID string
		want    plan.Origin
	}{
		{"leader explicit", "leader", plan.OriginLeader},
		{"leader implicit", "", plan.OriginLeader},
		{"project_manager", "project_manager-una", plan.OriginProjectManager},
		{"unknown role falls back to leader", "worker-ada", plan.OriginLeader},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := mcpgo.CallToolRequest{}
			req.Params.Name = "add_task"
			args := map[string]any{"title": "T " + c.name}
			if c.agentID != "" {
				args["agent_id"] = c.agentID
			}
			req.Params.Arguments = args
			res, err := srv.handleAddTask(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError {
				t.Fatalf("add_task error: %s", textOf(t, res))
			}
			var task plan.Task
			if err := json.Unmarshal([]byte(textOf(t, res)), &task); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if task.Origin != c.want {
				t.Errorf("origin = %q want %q", task.Origin, c.want)
			}
		})
	}
}

// TestAddTask_ExplicitOriginOverridesDefault verifies that the explicit
// origin argument wins over caller-role inference: a leader-caller can
// still file an operator-origin task on the user's behalf, and an
// unknown origin string is rejected with a clear error.
func TestAddTask_ExplicitOriginOverridesDefault(t *testing.T) {
	srv, _, _, _ := newTestServerFull(t)
	t.Run("operator override on leader caller", func(t *testing.T) {
		req := mcpgo.CallToolRequest{}
		req.Params.Name = "add_task"
		req.Params.Arguments = map[string]any{
			"title":    "operator asked",
			"agent_id": "leader",
			"origin":   "operator",
		}
		res, err := srv.handleAddTask(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatalf("error: %s", textOf(t, res))
		}
		var task plan.Task
		_ = json.Unmarshal([]byte(textOf(t, res)), &task)
		if task.Origin != plan.OriginOperator {
			t.Errorf("origin = %q want operator", task.Origin)
		}
	})
	t.Run("bogus origin rejected", func(t *testing.T) {
		req := mcpgo.CallToolRequest{}
		req.Params.Name = "add_task"
		req.Params.Arguments = map[string]any{
			"title":  "x",
			"origin": "manager",
		}
		res, err := srv.handleAddTask(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Error("invalid origin should error")
		}
	})
}

// TestAddTask_EmitsTaskCreatedAuditEvent verifies that the synthetic
// task_created audit event lands with the right meta keys so the
// TaskDetailModal can render the creation row even for tasks created
// after Origin shipped (the modal also falls back to task.created_at
// when the event is missing).
func TestAddTask_EmitsTaskCreatedAuditEvent(t *testing.T) {
	srv, p, _, a := newTestServerFull(t)

	parent, _ := p.AddTask(plan.NewTaskInput{Title: "parent"})

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "add_task"
	req.Params.Arguments = map[string]any{
		"title":     "follow-up",
		"agent_id":  "leader",
		"parent_id": parent.ID,
	}
	res, err := srv.handleAddTask(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("add_task error: %s", textOf(t, res))
	}
	var task plan.Task
	_ = json.Unmarshal([]byte(textOf(t, res)), &task)

	events, _ := a.Query("", parseZero(), 100)
	var got *audit.Event
	for i := range events {
		if events[i].Kind == audit.KindTaskCreated && events[i].Meta["task_id"] == task.ID {
			got = &events[i]
			break
		}
	}
	if got == nil {
		t.Fatal("no task_created event emitted")
	}
	if got.AgentID != "leader" {
		t.Errorf("event AgentID = %q want leader", got.AgentID)
	}
	if got.Meta["title"] != "follow-up" {
		t.Errorf("meta.title = %v want follow-up", got.Meta["title"])
	}
	if got.Meta["origin"] != "leader" {
		t.Errorf("meta.origin = %v want leader", got.Meta["origin"])
	}
	if got.Meta["parent_id"] != parent.ID {
		t.Errorf("meta.parent_id = %v want %s", got.Meta["parent_id"], parent.ID)
	}
	if got.Meta["agent_id"] != "leader" {
		t.Errorf("meta.agent_id = %v want leader", got.Meta["agent_id"])
	}
}
