package api

import (
	"context"
	"encoding/json"
)

// HeartbeatPath is where the relay's heartbeat goes: the API gateway, as the signed-in
// developer, for their organization — machine meta-telemetry belongs to no project.
//
// STUB: the endpoint is being built. The body is one OTLP/JSON logs export
// (terma.relay.heartbeat, internal/relay/heartbeat.go); until the gateway serves it, a
// send answers 404, which the relay counts and otherwise ignores.
const HeartbeatPath = "/v1/relay/heartbeat"

// SendHeartbeat posts one heartbeat, an OTLP/JSON logs export.
func (c *Client) SendHeartbeat(ctx context.Context, otlpJSON []byte) error {
	return c.Post(ctx, HeartbeatPath, json.RawMessage(otlpJSON), nil)
}
