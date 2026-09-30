package config

import (
	"slices"
	"time"
)

// Collection modes: what the organization the developer signed in to collects from
// this machine.
const (
	// ModeRepo collects what repositories opted in to: sessions a hook of a bound
	// repository claimed. Everything else stays on the machine. The default.
	ModeRepo = "repo"
	// ModeGlobal collects every session on the machine — an organization's choice for
	// company laptops where it wants all AI spend.
	ModeGlobal = "global"
)

// Policy is the organization's collection policy, fetched by `terma setup` from the
// account it signs in to and kept on the profile. Hooks read it locally; the relay
// refreshes it in the background. Project routing records can only narrow capture.
type Policy struct {
	Mode               string `json:"mode"`
	IncludePrompts     bool   `json:"include_prompts"`
	IncludeToolContent bool   `json:"include_tool_content"`
	// Nil signals means all for profiles written before the policy endpoint existed;
	// an explicitly empty list means collect nothing.
	Signals                   []string  `json:"signals"`
	ExcludePaths              []string  `json:"exclude_paths,omitempty"`
	MembersCanPause           bool      `json:"members_can_pause"`
	MembersCanAddRepositories bool      `json:"members_can_add_repositories"`
	Revision                  int64     `json:"revision"`
	UpdatedAt                 time.Time `json:"updated_at,omitempty"`
	OrganizationID            string    `json:"organization_id,omitempty"`
	TeamID                    string    `json:"team_id,omitempty"`
	AuthURL                   string    `json:"auth_url,omitempty"`
	// DefaultProjectID is the selected team's project in global mode: every native
	// export and hook event, including sessionless metrics and non-repository work.
	DefaultProjectID string    `json:"default_project_id,omitempty"`
	FetchedAt        time.Time `json:"fetched_at"`
}

// DefaultPolicy is what applies before `terma setup` has fetched one: repositories opt
// in, and prompts and tool content are collected unless a project turns them off.
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
// Empty scope fields support profiles written before policy fetching existed.
func (p Policy) AppliesTo(organizationID, authURL string) bool {
	return (p.OrganizationID == "" || p.OrganizationID == organizationID) &&
		(p.AuthURL == "" || p.AuthURL == authURL)
}
