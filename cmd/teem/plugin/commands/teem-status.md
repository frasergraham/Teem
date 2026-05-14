---
description: Show a one-glance status report for the current Teem.
---

Produce a tight status summary the operator can read at a glance. Pull
fresh data from the MCP tools below — don't rely on memory.

- `get_leader_status` — render the latest entry for `AgentID="leader"`
  (one line, plus `current_task_ids` if set).
- `list_agents` — for each agent: id, role, state, placement.
- `list_tasks` with `open_only=true` — for each open task: id, title,
  stage, assignee.
- Derived **in-flight count** — number of agents from `list_agents`
  whose state is `busy`. No separate tool.
- Pulse state — call `read_team` for config, and `query_audit` with
  `kinds=["pulse_tick"]` and a small `limit` to find the most recent
  tick (or absence). Report: running? paused? interval? last tick?

Format as a compact stacked summary — short headers and one-line rows,
not a wall of JSON. Lead with the leader status and in-flight count;
end with pulse. Skip empty sections silently.
