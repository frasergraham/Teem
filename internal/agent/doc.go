// Package agent wires a provisioned worker to the bus.
//
// A Worker subscribes to its own jobs topic on the bus, runs each job by
// shelling out to `claude -p` through its Transport, and publishes the
// result back on the bus. Spawner is the glue the orchestrator's MCP
// spawn_agent tool calls into; it looks up a role in the team YAML,
// places the worker via the Provisioner, registers it in the MCP
// Registry, and starts the Worker goroutine.
package agent
