package config

import (
	"os"
	"slices"
	"time"
)

// Collection modes: what the signed-in organization collects from this machine.
const (
	// ModeRepo, the default, collects only sessions a bound repository's hook claimed.
	ModeRepo = "repo"
	// ModeGlobal collects every session on the machine.
	ModeGlobal = "global"
)

// Policy is the organization's collection policy, fetched by `terma setup`; routing
// records can only narrow it.
type Policy struct {
	Mode               string `json:"mode"`
	IncludePrompts     bool   `json:"include_prompts"`
	IncludeToolContent bool   `json:"include_tool_content"`
	// Signals nil means all; an empty list means collect nothing.
	Signals                   []string  `json:"signals"`
	ExcludePaths              []string  `json:"exclude_paths,omitempty"`
	MembersCanPause           bool      `json:"members_can_pause"`
	MembersCanAddRepositories bool      `json:"members_can_add_repositories"`
	Revision                  int64     `json:"revision"`
	UpdatedAt                 time.Time `json:"updated_at,omitempty"`
	OrganizationID            string    `json:"organization_id,omitempty"`
	TeamID                    string    `json:"team_id,omitempty"`
	AuthURL                   string    `json:"auth_url,omitempty"`
	// DefaultProjectID receives everything in global mode.
	DefaultProjectID string    `json:"default_project_id,omitempty"`
	FetchedAt        time.Time `json:"fetched_at"`
}

// DefaultPolicy applies before `terma setup` has fetched one: repository mode, content on.
func DefaultPolicy() Policy {
	return Policy{Mode: ModeRepo, IncludePrompts: true, IncludeToolContent: true, MembersCanAddRepositories: true}
}

// AllowsSignal applies the organization's signal ceiling.
func (p Policy) AllowsSignal(signal string) bool {
	return p.Signals == nil || slices.Contains(p.Signals, signal)
}

// Global reports whether the organization collects every session on the machine.
func (p Policy) Global() bool { return p.Mode == ModeGlobal }

// AppliesTo keeps a cached policy within the login and environment that fetched it.
func (p Policy) AppliesTo(organizationID, authURL string) bool {
	return (p.OrganizationID == "" || p.OrganizationID == organizationID) &&
		(p.AuthURL == "" || p.AuthURL == authURL)
}

// NoPolicy is what applies to a login until its team's policy is validated: repositories
// opt in and nothing is collected.
func NoPolicy(organizationID, authURL string) Policy {
	return Policy{Mode: ModeRepo, Signals: []string{}, OrganizationID: organizationID, AuthURL: authURL}
}

// PolicyStub is the offline test override, TERMA_POLICY_STUB, read here and nowhere else.
func PolicyStub() string { return os.Getenv("TERMA_POLICY_STUB") }

// Validated reports whether p is a team's fetched policy; an offline stub stands in for
// the team.
func (p Policy) Validated() bool {
	return !p.FetchedAt.IsZero() && (p.TeamID != "" || PolicyStub() != "")
}
