---
description: List the Teem's current agents and their states.
---

Use the `list_agents` MCP tool to show me every active agent, its role,
backend (local/ssh/fargate), lifecycle (ephemeral/persistent), and state
(provisioning/running/busy/stopped). Format as a compact table. If no
agents are running, call `read_team` and list the roster so I can pick
who to spawn.
