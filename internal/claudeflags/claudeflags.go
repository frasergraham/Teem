// Package claudeflags centralises the argv fragment that subscribes a
// Claude Code subprocess to the team MCP server's channel stream
// (`notifications/claude/channel`). The "teem" token references the
// key under `mcpServers` in claude-mcp.json / pulse-mcp.json.
//
// Channels remain preview-gated upstream: until the capability is
// approved, Claude Code requires `--dangerously-load-development-channels`
// for non-allowlisted server names. Set TEEM_CHANNELS_DEV=1 to opt in
// to the dev flag; without it the production `--channels` form is used
// and claude rejects it gracefully if unsupported.
package claudeflags

import "os"

// ChannelFlags returns the argv segment for `claude` that opts the
// process into the team MCP server's channel stream.
func ChannelFlags() []string {
	const token = "server:teem"
	if os.Getenv("TEEM_CHANNELS_DEV") == "1" {
		return []string{"--dangerously-load-development-channels", token}
	}
	return []string{"--channels", token}
}
