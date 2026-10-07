package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PoliciesDir names the state directory's folder of collection policies, one file per
// team: terma downloads and rewrites them as it runs.
const PoliciesDir = "policies"

// PolicyPath is team's policy file under the state directory stateDir.
func PolicyPath(stateDir, team string) (string, error) {
	// A team ID names a file, so nothing that could step out of the folder; config cannot
	// import project.ValidID (project's tests import session, which imports config).
	if team == "" || team != filepath.Base(team) || strings.HasPrefix(team, ".") {
		return "", errors.New("invalid policy team ID")
	}
	return filepath.Join(stateDir, PoliciesDir, team+".json"), nil
}

// ReadPolicy is team's validated policy stored under stateDir; false when there is none.
func ReadPolicy(stateDir, team string) (Policy, bool, error) {
	if stateDir == "" || team == "" {
		return Policy{}, false, nil
	}
	path, err := PolicyPath(stateDir, team)
	if err != nil {
		return Policy{}, false, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Policy{}, false, nil
	}
	if err != nil {
		return Policy{}, false, err
	}
	var p Policy
	if err := json.Unmarshal(b, &p); err != nil {
		return Policy{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	if p.TeamID != team || p.FetchedAt.IsZero() || p.Mode != ModeRepo && p.Mode != ModeGlobal {
		return Policy{}, false, fmt.Errorf("invalid collection policy in %s", path)
	}
	return p, true, nil
}

// ListPolicies is every validated policy stored under stateDir, one per team, in team
// order; a file that is not one is skipped, as ReadPolicy would refuse it.
func ListPolicies(stateDir string) ([]Policy, error) {
	if stateDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(filepath.Join(stateDir, PoliciesDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Policy
	for _, e := range entries {
		team, ok := strings.CutSuffix(e.Name(), ".json")
		if e.IsDir() || !ok {
			continue
		}
		p, ok, err := ReadPolicy(stateDir, team)
		if err != nil || !ok {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// WritePolicy stores p as its team's under stateDir; callers serialize writes
// (routing.StorePolicy).
func WritePolicy(stateDir string, p Policy) error {
	path, err := PolicyPath(stateDir, p.TeamID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, b, 0o600)
}

// Collection modes: what the signed-in organization collects from this machine.
const (
	// ModeRepo, the default, collects only the repositories the policy lists.
	ModeRepo = "repo"
	// ModeGlobal collects every session on the machine.
	ModeGlobal = "global"
)

// Policy is the organization's collection policy, fetched by `terma setup`.
type Policy struct {
	Mode string `json:"mode"`
	// Repositories are what repository mode admits (Admits).
	Repositories       []string `json:"repositories,omitempty"`
	IncludePrompts     bool     `json:"include_prompts"`
	IncludeToolContent bool     `json:"include_tool_content"`
	// GitHooks has terma install its commit hooks in the repositories the policy collects.
	GitHooks bool `json:"git_hooks"`
	// Unset is a team no admin has given a collection policy yet: it collects nothing.
	Unset bool `json:"unset,omitempty"`
	// CollectsNothing is set while no validated policy applies: nothing leaves the machine.
	CollectsNothing bool      `json:"collects_nothing,omitempty"`
	Revision        int64     `json:"revision"`
	UpdatedAt       time.Time `json:"updated_at"`
	OrganizationID  string    `json:"organization_id,omitempty"`
	TeamID          string    `json:"team_id,omitempty"`
	AuthURL         string    `json:"auth_url,omitempty"`
	// OrganizationName and TeamName are what setup knew the organization and team as, for
	// doctor to name them; a refresh keeps the stored ones.
	OrganizationName string `json:"organization_name,omitempty"`
	TeamName         string `json:"team_name,omitempty"`
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

// PolicyStaleAfter is how old a policy grows only when no relay is refreshing it, which a
// running relay does every minute.
const PolicyStaleAfter = 2 * time.Minute

// Stale reports whether p is validated but more than PolicyStaleAfter old.
func (p Policy) Stale(now time.Time) bool {
	return p.Validated() && now.Sub(p.FetchedAt) > PolicyStaleAfter
}

// Team is the team whose policy p is: its own, else global mode's default project.
func (p Policy) Team() string {
	if p.TeamID == "" && p.Global() {
		return p.DefaultProjectID
	}
	return p.TeamID
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
	return (p.OrganizationID == "" || p.OrganizationID == organizationID) && p.SameEnvironment(authURL)
}

// SameEnvironment keeps a cached policy within the environment that fetched it: a key
// minted at one auth host is nothing to another. Which organization fetched it is the
// policy's own to say (OrganizationID): one machine collects for several at once.
func (p Policy) SameEnvironment(authURL string) bool {
	return p.AuthURL == "" || p.AuthURL == authURL
}

// Label names p's team for a person: its name, else its id, with its organization.
func (p Policy) Label() string {
	team := p.TeamName
	if team == "" {
		team = p.Team()
	}
	switch org := firstNonEmpty(p.OrganizationName, p.OrganizationID); {
	case team == "":
		return org
	case org == "":
		return "team " + team
	}
	return "team " + team + " of " + firstNonEmpty(p.OrganizationName, p.OrganizationID)
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
