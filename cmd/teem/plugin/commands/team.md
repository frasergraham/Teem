---
description: Show the team roster from the YAML configuration.
---

Use the `read_team` MCP tool to fetch the team configuration and
summarize it for me:

- Team name and a one-line read of the leader brief.
- Each agent's id, role, description, placement (local/ssh/fargate),
  and lifecycle (ephemeral/persistent).
- Cross-reference `list_agents` to mark which roster members are
  currently running.

Format as a compact table or list. Don't include the full leader
system prompt unless I ask.
