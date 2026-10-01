package api

import (
	"context"
	"encoding/json"
)

// HeartbeatPath is where the relay's heartbeat goes, under the developer's credential;
// the gateway does not serve it yet, so a send answers 404.
const HeartbeatPath = "/v1/relay/heartbeat"

// SendHeartbeat posts one heartbeat, an OTLP/JSON logs export.
func (c *Client) SendHeartbeat(ctx context.Context, otlpJSON []byte) error {
	return c.Post(ctx, HeartbeatPath, json.RawMessage(otlpJSON), nil)
}
