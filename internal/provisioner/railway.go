package provisioner

import "context"

// RailwayProvisioner deploys workers as Railway services so the team can
// scale beyond the operator's local machine and SSH-reachable boxes.
//
// Status: STUB. Returns ErrNotImplemented. The interface shape is committed
// so callers can wire `--backend=railway` without breaking; the
// implementation lands alongside the teem-worker container image in a
// follow-up PR.
//
// # Design (for the follow-up PR)
//
// Config — read from env or team YAML:
//   - RAILWAY_TOKEN          — account/project token for GraphQL.
//   - RAILWAY_PROJECT_ID     — target project.
//   - RAILWAY_ENVIRONMENT_ID — target environment ("production" etc).
//   - TEEM_WORKER_IMAGE      — container image (default
//     ghcr.io/frasergraham/teem-worker:<version>).
//   - TS_AUTHKEY             — tailnet ephemeral auth key, injected into
//     the container so it joins the same tailnet as the Leader.
//
// Provision flow (all calls against https://backboard.railway.app/graphql/v2):
//  1. serviceCreate(projectId, environmentId, name="teem-<agent-id>",
//     source={image: TEEM_WORKER_IMAGE})
//  2. variableUpsert for each of:
//     TS_AUTHKEY, TEEM_LEADER_HOST, TEEM_AGENT_ID, TEEM_AGENT_ROLE,
//     ANTHROPIC_API_KEY, TEEM_BUS_URL.
//  3. serviceInstanceDeploy(serviceId, environmentId).
//  4. Poll deployments(serviceId) until status==SUCCESS.
//  5. Wait for the worker's tailnet hostname to appear on the bus — the
//     container registers itself via a KindStatus message once tsnet is up;
//     Railway never tells us the tailnet hostname directly.
//  6. Return &Agent{Backend: BackendRailway, TailnetHost: <hostname>,
//     Transport: tailnetExecTransport{...}}.
//
// Teardown: serviceDelete(serviceId). Idempotent — a missing service is
// treated as success.
//
// What still has to land before this is end-to-end:
//   - cmd/teem-worker: a small helper that embeds tsnet, subscribes to the
//     bus, and runs `claude` against a leader-supplied MCP config.
//   - A worker→leader transport so the Leader can dispatch jobs over the
//     tailnet (today the bus is in-process).
//   - The published container image.
type RailwayProvisioner struct{}

func (RailwayProvisioner) Provision(_ context.Context, _ AgentSpec) (*Agent, error) {
	return nil, ErrNotImplemented
}

func (RailwayProvisioner) Teardown(_ context.Context, _ *Agent) error {
	return ErrNotImplemented
}
