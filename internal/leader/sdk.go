package leader

import (
	"context"
	"errors"
)

// ErrSDKNotImplemented is returned by SDKLeader stubs until the in-process
// Agent SDK backend lands. The ClaudeLeader CLI backend already covers the
// "Leader runs remotely, feels local" use case via the SSH transport, so
// this is a low-priority follow-up.
var ErrSDKNotImplemented = errors.New("leader: SDK backend not implemented (use ClaudeLeader)")

// SDKLeader will host the Leader using the Anthropic Agent SDK library
// (Python or TS via a sidecar) instead of the `claude` CLI. The interface
// is committed so callers can wire `--leader-backend=sdk` without
// breaking; full impl in a follow-up.
type SDKLeader struct{}

func (*SDKLeader) Send(_ context.Context, _ string) error { return ErrSDKNotImplemented }
func (*SDKLeader) Events() <-chan AssistantEvent {
	ch := make(chan AssistantEvent)
	close(ch)
	return ch
}
func (*SDKLeader) Close() error { return nil }
