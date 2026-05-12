// Package mcp implements the orchestrator's MCP server.
//
// The Leader (a `claude` subprocess) connects to this server as an MCP
// client; the tools registered here are how the Leader spawns agents,
// assigns jobs, inspects status, and reads the team roster. The server
// is mounted on a tailnet-scoped listener so workers on other hosts can
// reach it too.
package mcp
