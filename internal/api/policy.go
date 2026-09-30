package api

import (
	"context"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// CollectionPolicy asks the account service what the signed-in organization collects
// from this machine — every session (global) or what repositories opted in to (repo) —
// and its content defaults.
//
// STUB: the endpoint is being built on the account service. Until it lands this answers
// the default policy without a request. When it does, this becomes a GET with the user
// credential, and an organization that has set no policy answers the default too.
func (c *Client) CollectionPolicy(ctx context.Context) (config.Policy, error) {
	if err := ctx.Err(); err != nil {
		return config.Policy{}, err
	}
	p := config.DefaultPolicy()
	p.FetchedAt = time.Now().UTC()
	return p, nil
}
