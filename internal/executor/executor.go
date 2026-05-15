// Package executor abstracts "run one job and return the final assistant
// text." Implementations:
//   - ProcessExecutor: shells out to `claude -p ...` via a transport.Transport
//     (local or SSH). Used for in-process workers the leader owns.
//   - HTTPExecutor: POSTs the job to a remote teem-worker daemon over the
//     tailnet and long-polls for the result. Used for cloud-backed agents.
//
// The package lives outside internal/agent so that internal/provisioner can
// reference Executor types without forming an import cycle with the spawner.
package executor

import (
	"context"

	"github.com/frasergraham/teem/internal/team"
)

// Job is the unit of work passed to an Executor. It mirrors the on-bus
// jobMessage that the spawner publishes — kept here so the bus topic schema
// stays in one place (internal/agent) and Executors stay decoupled from it.
type Job struct {
	ID      string        `json:"job_id"`
	Prompt  string        `json:"prompt"`
	Context string        `json:"context,omitempty"`
	MCPs    []team.MCPRef `json:"mcps,omitempty"`
	// Skill names a Claude Code skill the executor should ask claude
	// to invoke. ProcessExecutor passes it via --append-system-prompt
	// since claude has no dedicated --load-skill flag. Empty disables.
	Skill string `json:"skill,omitempty"`
}

// Executor runs a single job and returns the final assistant text (or an
// error). Implementations should respect ctx cancellation.
type Executor interface {
	Execute(ctx context.Context, job Job) (output string, err error)
}
