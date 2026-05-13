---
description: Read the Teem audit log. Usage: /audit [agent-id]
---

Use the `query_audit` MCP tool to fetch recent events from the audit log.

- If $ARGUMENTS names an agent (e.g. `be-1`), pass it as `agent_id` to
  restrict the query.
- Otherwise fetch across all agents.
- Default `limit` is 50; use `since` to scope to a recent window when
  the request implies one (e.g. "in the last hour" → since = now - 1h).

Summarize the events for me — group by agent and job, surface any
errors or git pushes prominently. Don't just dump the raw JSON.
