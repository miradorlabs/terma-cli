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
// this machine, with the developer's login or the team's own server key;
// TERMA_POLICY_STUB is an offline test override. It reads major 1 alone: a reader that
// matches entries exactly would read a 2.x wildcard as a repository no origin names, and
// collect less than the admin set, so it keeps its last validated policy instead.
func (c *Client) CollectionPolicy(ctx context.Context) (config.Policy, error) {
	return c.collectionPolicy(ctx, false)
}

// TeamPolicy is CollectionPolicy for a reader that routes by routing.ParseEntry, which
// matches wildcards: it reads majors 1 and 2, and sets Wildcards on a 2.x policy, whose
// entries alone may end in `/*` (C1: the backend stores each policy in the lowest major
// that expresses it).
func (c *Client) TeamPolicy(ctx context.Context) (config.Policy, error) {
	return c.collectionPolicy(ctx, true)
}

func (c *Client) collectionPolicy(ctx context.Context, wildcards bool) (config.Policy, error) {
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
	// A server key is bound to its team and names none; a login names the team it reads.
	query := url.Values{}
	if c.apiKey == "" {
		if c.projectID == "" {
			return config.Policy{}, fmt.Errorf("collection policy requires a team ID")
		}
		query.Set("project_id", c.projectID)
	}
	if err := c.AuthGet(ctx, "/v1/policy", query, &response); err != nil {
		return config.Policy{}, err
	}
	return response.collectionPolicy(wildcards)
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

// collectionPolicy reads the response as a policy, of major 1 alone unless wildcards.
func (r policyResponse) collectionPolicy(wildcards bool) (config.Policy, error) {
	// A team no admin has set a policy for admits no repository.
	if r.Policy == nil {
		p := config.DefaultPolicy()
		p.Unset = true
		p.FetchedAt = time.Now().UTC()
		return p, nil
	}
	t := r.Policy.Terma
	c := t.Capture
	// Only a major version a reader does not know makes a document unreadable; a minor
	// one only adds fields it may ignore.
	major, _, dotted := strings.Cut(r.Policy.Version, ".")
	known := major == "1" || wildcards && major == "2"
	if !dotted || !known || (t.Global == nil) == (t.PerRepository == nil) ||
		t.PerRepository != nil && t.PerRepository.Repositories == nil ||
		c == nil || c.ExcludePrompts == nil || c.ExcludeToolContent == nil ||
		r.Revision < 1 || r.UpdatedAt.IsZero() {
		versions := "a 1.x version"
		if wildcards {
			versions = "a 1.x or 2.x version"
		}
		return config.Policy{}, fmt.Errorf("invalid collection policy: require %s, one collection mode, a repository list and complete capture rules", versions)
	}
	p := config.Policy{Mode: config.ModeRepo, IncludePrompts: !*c.ExcludePrompts,
		IncludeToolContent: !*c.ExcludeToolContent, GitHooks: t.GitHooks != nil && *t.GitHooks,
		Revision: r.Revision, UpdatedAt: r.UpdatedAt, FetchedAt: time.Now().UTC(), Wildcards: major == "2"}
	if t.Global != nil {
		p.Mode = config.ModeGlobal
	} else {
		p.Repositories = *t.PerRepository.Repositories
	}
	return p, nil
}
