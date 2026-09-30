package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// CollectionPolicy asks the account service what the signed-in organization collects
// from this machine — every session (global) or what repositories opted in to (repo) —
// its content defaults and, for global mode, where sessions no binding places go.
//
// STUB: the endpoint is being built on the account service. Until it lands this answers
// the default policy without a request, or, for tests and trials, the policy JSON in
// TERMA_POLICY_STUB (the fields of config.Policy). When it does, this becomes a GET
// with the user credential, and an organization that has set no policy answers the
// default too.
func (c *Client) CollectionPolicy(ctx context.Context) (config.Policy, error) {
	if err := ctx.Err(); err != nil {
		return config.Policy{}, err
	}
	p := config.DefaultPolicy()
	if raw := os.Getenv("TERMA_POLICY_STUB"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return config.Policy{}, fmt.Errorf("TERMA_POLICY_STUB: %w", err)
		}
		if p.Mode != config.ModeRepo && p.Mode != config.ModeGlobal {
			return config.Policy{}, fmt.Errorf("TERMA_POLICY_STUB: mode %q: want repo or global", p.Mode)
		}
	}
	p.FetchedAt = time.Now().UTC()
	return p, nil
}
