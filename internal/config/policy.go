package config

import (
	"os"
	"time"
)

// Collection modes: what the signed-in organization collects from this machine.
const (
	// ModeRepo, the default, collects only the folders the policy lists.
	ModeRepo = "repo"
	// ModeGlobal collects every session on the machine.
	ModeGlobal = "global"
)

// Policy is the organization's collection policy, fetched by `terma setup`.
type Policy struct {
	Mode string `json:"mode"`
	// Folders are what repository mode admits (Admits).
	Folders            []string `json:"folders,omitempty"`
	IncludePrompts     bool     `json:"include_prompts"`
	IncludeToolContent bool     `json:"include_tool_content"`
	// CollectsNothing is set while no validated policy applies: nothing leaves the machine.
	CollectsNothing bool      `json:"collects_nothing,omitempty"`
	Revision        int64     `json:"revision"`
	UpdatedAt       time.Time `json:"updated_at"`
	OrganizationID  string    `json:"organization_id,omitempty"`
	TeamID          string    `json:"team_id,omitempty"`
	AuthURL         string    `json:"auth_url,omitempty"`
	// DefaultProjectID receives everything in global mode.
	DefaultProjectID string    `json:"default_project_id,omitempty"`
	FetchedAt        time.Time `json:"fetched_at"`
}

// MaxPolicyAge is how long a validated policy governs collection without a successful
// refresh; after it, nothing is collected until a refresh succeeds.
const MaxPolicyAge = 7 * 24 * time.Hour

// Expired reports whether p was validated more than MaxPolicyAge before now.
func (p Policy) Expired(now time.Time) bool {
	return !p.FetchedAt.IsZero() && now.Sub(p.FetchedAt) > MaxPolicyAge
}

// DefaultPolicy applies before `terma setup` has fetched one: repository mode, content on.
func DefaultPolicy() Policy {
	return Policy{Mode: ModeRepo, IncludePrompts: true, IncludeToolContent: true}
}

// Content is what content the team's policy lets a session carry, the one rule the relay
// and the hook events both apply: a policy not validated withholds all.
func (p Policy) Content() (prompts, toolContent bool) {
	if p.CollectsNothing {
		return false, false
	}
	return p.IncludePrompts, p.IncludeToolContent
}

// Global reports whether the organization collects every session on the machine.
func (p Policy) Global() bool { return p.Mode == ModeGlobal }

// AppliesTo keeps a cached policy within the login and environment that fetched it.
func (p Policy) AppliesTo(organizationID, authURL string) bool {
	return (p.OrganizationID == "" || p.OrganizationID == organizationID) &&
		(p.AuthURL == "" || p.AuthURL == authURL)
}

// NoPolicy is what applies to a login until its team's policy is validated: no repository
// is admitted and nothing is collected.
func NoPolicy(organizationID, authURL string) Policy {
	return Policy{Mode: ModeRepo, CollectsNothing: true, OrganizationID: organizationID, AuthURL: authURL}
}

// PolicyStub is the offline test override, TERMA_POLICY_STUB, read here and nowhere else.
func PolicyStub() string { return os.Getenv("TERMA_POLICY_STUB") }

// InForce is the policy hooks apply: p once validated, else NoPolicy.
func (p Policy) InForce(organizationID, authURL string) Policy {
	if p.Validated() {
		return p
	}
	return NoPolicy(organizationID, authURL)
}

// Validated reports whether p is a team's fetched policy; an offline stub stands in for
// the team.
func (p Policy) Validated() bool {
	return !p.FetchedAt.IsZero() && (p.TeamID != "" || PolicyStub() != "")
}
