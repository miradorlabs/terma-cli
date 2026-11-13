package migrate

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// Organization types, as GET /v1/organizations names them (Machine.OrgType).
const (
	// OrgPersonal is a person's own workspace: its team migrates as personal, unbound, and
	// its global policy keeps collecting everything not bound elsewhere.
	OrgPersonal = "personal"
	// OrgShared is any other organization: its entries are bound to it, and its global
	// policy collects nothing.
	OrgShared = "shared"
)

// orgTypeWait bounds each organization type question, asked before any lock is taken.
const orgTypeWait = 10 * time.Second

// errOrgTypeUnknown is a team whose organization type was not learned before the locks,
// as when config.json changed meanwhile.
var errOrgTypeUnknown = errors.New("the organization type of a profile's team is not known")

// orgTypes asks, before the locks, whether each profile's team is a personal
// organization's, with that profile's sign-in (Machine.OrgType), keyed by orgKey. A
// server-key profile has no sign-in, and migrates as an organization's. Any answer missing
// is an error: today's rules stay until the next relay start.
func (m Machine) orgTypes(ctx context.Context) (map[string]bool, error) {
	file, err := config.LoadFile(m.ConfigDir)
	if err != nil {
		return nil, err
	}
	personal := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(file.Profiles)) {
		p := file.Profiles[name]
		team, err := m.legacyTeam(name, p, false)
		switch {
		case err != nil:
			return nil, fmt.Errorf("profile %s: %w", name, err)
		case team == nil:
			continue
		case p.ServerKeySignIn:
			personal[orgKey(name, team.OrganizationID)] = false
			continue
		case m.OrgType == nil:
			return nil, fmt.Errorf("profile %s: no way to ask organization %s's type", name, team.OrganizationID)
		}
		asked, cancel := context.WithTimeout(ctx, orgTypeWait)
		kind, err := m.OrgType(asked, name, team.OrganizationID)
		cancel()
		switch {
		case err != nil:
			return nil, fmt.Errorf("profile %s: ask organization %s's type with its sign-in: %w", name, team.OrganizationID, err)
		case kind != OrgPersonal && kind != OrgShared:
			return nil, fmt.Errorf("profile %s: organization %s's type is %q, neither personal nor shared", name, team.OrganizationID, kind)
		}
		personal[orgKey(name, team.OrganizationID)] = kind == OrgPersonal
	}
	return personal, nil
}

// orgKey keys an organization type the migration learned by the profile that asked.
func orgKey(profile, organizationID string) string { return profile + "\x00" + organizationID }
