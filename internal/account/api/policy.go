package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// CollectionPolicy asks the account service what the signed-in organization collects from
// this machine; TERMA_POLICY_STUB is an offline test override.
func (c *Client) CollectionPolicy(ctx context.Context) (config.Policy, error) {
	if err := ctx.Err(); err != nil {
		return config.Policy{}, err
	}
	p := config.DefaultPolicy()
	if raw := config.PolicyStub(); raw != "" {
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return config.Policy{}, fmt.Errorf("TERMA_POLICY_STUB: %w", err)
		}
		if p.Mode != config.ModeRepo && p.Mode != config.ModeGlobal {
			return config.Policy{}, fmt.Errorf("TERMA_POLICY_STUB: mode %q: want repo or global", p.Mode)
		}
		p.FetchedAt = time.Now().UTC()
		return p, nil
	}
	var response policyResponse
	if c.apiKey != "" {
		return config.Policy{}, fmt.Errorf("collection policy requires a developer login; server keys only deliver telemetry")
	}
	if c.projectID == "" {
		return config.Policy{}, fmt.Errorf("collection policy requires a team ID")
	}
	if err := c.AuthGet(ctx, "/v1/policy", url.Values{"project_id": {c.projectID}}, &response); err != nil {
		return config.Policy{}, err
	}
	return response.collectionPolicy()
}

// policyResponse uses pointers so a missing capture switch never reads as an authorization.
type policyResponse struct {
	Policy *struct {
		Version string `json:"version"`
		Terma   struct {
			Capture *struct {
				ExcludePrompts     *bool `json:"exclude_prompts"`
				ExcludeToolContent *bool `json:"exclude_tool_content"`
			} `json:"capture"`
			// GitHooks is a pointer because a 1.0 server does not send it, and absent
			// means false: unlike the capture switches it authorizes nothing.
			GitHooks *bool `json:"git_hooks"`
			// Which of the two is present selects the mode.
			Global        *struct{} `json:"global"`
			PerRepository *struct {
				// Repositories is required: a missing list is not "every repository".
				Repositories *[]string `json:"repositories"`
			} `json:"per_repository"`
		} `json:"terma"`
	} `json:"policy"`
	Revision  int64     `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (r policyResponse) collectionPolicy() (config.Policy, error) {
	// An organization that never set a policy admits no repository.
	if r.Policy == nil {
		p := config.DefaultPolicy()
		p.FetchedAt = time.Now().UTC()
		return p, nil
	}
	t := r.Policy.Terma
	c := t.Capture
	// Only a major version a reader does not know makes a document unreadable; a minor
	// one only adds fields it may ignore.
	major, _, dotted := strings.Cut(r.Policy.Version, ".")
	if !dotted || major != "1" || (t.Global == nil) == (t.PerRepository == nil) ||
		t.PerRepository != nil && t.PerRepository.Repositories == nil ||
		c == nil || c.ExcludePrompts == nil || c.ExcludeToolContent == nil ||
		r.Revision < 1 || r.UpdatedAt.IsZero() {
		return config.Policy{}, fmt.Errorf("invalid collection policy: require a 1.x version, one collection mode, a repository list and complete capture rules")
	}
	p := config.Policy{Mode: config.ModeRepo, IncludePrompts: !*c.ExcludePrompts,
		IncludeToolContent: !*c.ExcludeToolContent, GitHooks: t.GitHooks != nil && *t.GitHooks,
		Revision: r.Revision, UpdatedAt: r.UpdatedAt, FetchedAt: time.Now().UTC()}
	if t.Global != nil {
		p.Mode = config.ModeGlobal
	} else {
		p.Repositories = *t.PerRepository.Repositories
	}
	return p, nil
}
