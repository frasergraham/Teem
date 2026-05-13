---
description: Spawn a teem worker for the given role. Usage: /teem-spawn <role>
---

If a role was provided ($ARGUMENTS), use the `spawn_agent` MCP tool to
spawn it now. Report the new `agent_id` and whether the worker is
provisioning (Fargate cold start) or already running (local).

If no role was provided, call `read_team` and ask me which role to spawn,
listing the available ones with their descriptions.

Do not assign a job in this turn — just spawn and tell me when ready.
