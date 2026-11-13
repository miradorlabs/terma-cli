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
	// OrgPersonal is a person's own workspace: its team migrates as personal, with no
	// bindings, and a global policy there keeps collecting everything not bound elsewhere.
	OrgPersonal = "personal"
	// OrgShared is every other organization: its team migrates with its entries bound to
	// it, and a global policy there collects nothing.
	OrgShared = "shared"
)

// orgTypeWait bounds each organization type the migration asks for: it asks before it
// takes the spool flush's lock, while the relay that runs it is not listening yet.
const orgTypeWait = 10 * time.Second

// errOrgTypeUnknown is a profile's team whose organization type the migration did not
// learn before it took its locks, as when config.json changed meanwhile.
var errOrgTypeUnknown = errors.New("the organization type of a profile's team is not known")

// orgTypes asks, before the migration takes its locks, whether each profile's team is a
// personal organization's, with that profile's own sign-in to it (Machine.OrgType): the
// answers are keyed by orgKey. A profile set up with a server key has no sign-in to ask
// with; it stays single-team, and its team migrates as an organization's. Any type that
// cannot be had is an error: the migration then waits for the next relay start, and the
// machine keeps today's rules meanwhile.
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
