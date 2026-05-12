// Package team loads and validates Teem team configuration from YAML.
//
// A team is the static description of the Leader plus a roster of agents
// (workers). It is consumed by the orchestrator at startup and exposed
// through the read_team MCP tool so the Leader can reason about who is on
// the team and how to dispatch work.
package team
