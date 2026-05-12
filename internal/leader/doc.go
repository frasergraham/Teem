// Package leader runs the Leader claude-code session that drives the
// team.
//
// The Leader is an Anthropic Agent SDK harness in disguise — `claude -p`
// is the SDK exposed as a stream-json stdio process. By dispatching the
// process through a transport.Transport, the Leader can live on the
// local machine (LocalTransport), a remote SSH host (SSHTransport), or a
// future Railway container. The chat UI in cmd/teem only talks to the
// Leader interface, so swapping backends doesn't change the operator's
// experience: it always feels like a local chat window.
package leader
