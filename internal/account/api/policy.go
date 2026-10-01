package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
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
	if raw := os.Getenv("TERMA_POLICY_STUB"); raw != "" {
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
		return config.Policy{}, fmt.Errorf("collection policy requires a team project ID")
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
				ExcludePrompts     *bool     `json:"exclude_prompts"`
				ExcludeToolContent *bool     `json:"exclude_tool_content"`
				ExcludePaths       *[]string `json:"exclude_paths"`
				Signals            *[]string `json:"signals"`
			} `json:"capture"`
			Global *struct {
				MembersCanPause bool `json:"members_can_pause"`
			} `json:"global"`
			PerRepository *struct {
				MembersCanAddRepositories bool `json:"members_can_add_repositories"`
			} `json:"per_repository"`
		} `json:"terma"`
	} `json:"policy"`
	Revision  int64     `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (r policyResponse) collectionPolicy() (config.Policy, error) {
	// An organization that never set a policy keeps repository opt-in.
	if r.Policy == nil {
		p := config.DefaultPolicy()
		p.FetchedAt = time.Now().UTC()
		return p, nil
	}
	t := r.Policy.Terma
	c := t.Capture
	if r.Policy.Version != "1.0" || (t.Global == nil) == (t.PerRepository == nil) ||
		c == nil || c.ExcludePrompts == nil || c.ExcludeToolContent == nil || c.ExcludePaths == nil || c.Signals == nil ||
		r.Revision < 1 || r.UpdatedAt.IsZero() {
		return config.Policy{}, fmt.Errorf("invalid collection policy: require version 1.0, one collection mode and complete capture rules")
	}
	for _, signal := range *c.Signals {
		if signal != "traces" && signal != "logs" && signal != "metrics" {
			return config.Policy{}, fmt.Errorf("invalid collection policy signal %q", signal)
		}
	}
	for _, path := range *c.ExcludePaths {
		if strings.TrimSpace(path) == "" {
			return config.Policy{}, fmt.Errorf("invalid empty excluded path")
		}
	}
	p := config.Policy{Mode: config.ModeRepo, IncludePrompts: !*c.ExcludePrompts,
		IncludeToolContent: !*c.ExcludeToolContent, Signals: append([]string{}, (*c.Signals)...),
		ExcludePaths: *c.ExcludePaths, Revision: r.Revision, UpdatedAt: r.UpdatedAt,
		FetchedAt: time.Now().UTC()}
	if t.Global != nil {
		p.Mode, p.MembersCanPause = config.ModeGlobal, t.Global.MembersCanPause
	} else {
		p.MembersCanAddRepositories = t.PerRepository.MembersCanAddRepositories
	}
	return p, nil
}
