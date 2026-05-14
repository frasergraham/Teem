// Package claudeflags centralises the argv fragment that subscribes a
// Claude Code subprocess to the channel notifications stream
// (`notifications/claude/channel`). The `teem-channel` token
// references the stdio MCP server registered under that key in
// claude-mcp.json / pulse-mcp.json — Claude Code only fires channel
// listeners on stdio servers it spawned itself, so the leader
// subscribes to the dedicated `teem-channel` shim rather than to the
// HTTP `teem` orchestrator.
//
// Channels remain preview-gated upstream: until the capability is
// approved, Claude Code requires `--dangerously-load-development-channels`
// for non-allowlisted server names. Set TEEM_CHANNELS_DEV=1 to opt in
// to the dev flag; without it the production `--channels` form is used
// and claude rejects it gracefully if unsupported.
package claudeflags

import "os"

// ChannelFlags returns the argv segment for `claude` that opts the
// process into the team's channel stream.
func ChannelFlags() []string {
	const token = "server:teem-channel"
	if os.Getenv("TEEM_CHANNELS_DEV") == "1" {
		return []string{"--dangerously-load-development-channels", token}
	}
	return []string{"--channels", token}
}
