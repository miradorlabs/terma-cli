package migrate

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// prepare writes every profile's routing state in the Preparing phase (step 2), and
// removes the Preparing states of profiles config.json no longer has, which an earlier
// start left: committed, they would hold a team nobody selects. The machine is not
// migrated yet, so nothing reads them.
func (m Machine) prepare() error {
	file, err := config.LoadFile(m.ConfigDir)
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(file.Profiles)) {
		if err := m.step("prepare/"+name, func() error { return m.prepareProfile(name, file.Profiles[name]) }); err != nil {
			return fmt.Errorf("profile %s: %w", name, err)
		}
	}
	states, err := preparing(m.StateDir)
	if err != nil {
		return err
	}
	for _, s := range states {
		if _, ok := file.Profiles[s.Profile]; ok {
			continue
		}
		path, err := routing.StatePath(m.StateDir, s.Profile)
		if err != nil {
			return err
		}
		if err := flock.Locked(path, 5*time.Second, func() error { return os.Remove(path) }); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// prepareProfile writes profile name's routing state in the Preparing phase: its team,
// if it has one, as the one enrollment, with the bindings its validated entries need and
// its legacy project mapped to it. A state an earlier start prepared is prepared again
// from config.json as it is now, keeping its enrollment while the team is the same; a
// state this migration did not prepare is the developer's, and is left as it is.
func (m Machine) prepareProfile(name string, p *config.Profile) error {
	team, err := m.legacyTeam(name, p)
	if err != nil {
		return err
	}
	_, found, err := routing.Load(m.StateDir, name)
	if err != nil {
		return err
	}
	write := routing.Create
	if found {
		write = routing.Update
	}
	_, err = write(m.StateDir, name, func(s *routing.State) error {
		if found && (s.Migration == nil || s.Migration.Phase != routing.Preparing) {
			return nil
		}
		return prepared(s, team)
	})
	return err
}

// prepared makes s the Preparing state for team, nil for a profile with none.
func prepared(s *routing.State, team *routing.Team) error {
	id := ""
	if team != nil {
		for _, t := range s.Teams {
			if t.TeamID == team.TeamID {
				id = t.EnrollmentID
			}
		}
		if id == "" {
			var err error
			if id, err = newEnrollmentID(); err != nil {
				return err
			}
		}
	}
	s.Teams, s.Bindings, s.Migration = nil, nil, &routing.Migration{Phase: routing.Preparing}
	if team == nil {
		return nil
	}
	t := *team
	t.EnrollmentID = id
	s.Teams = []routing.Team{t}
	s.Migration.Legacy = map[string]string{t.TeamID: id}
	// The team's validated entries are bound to its organization, an owner binding for a
	// wildcard and a repository binding for an exact entry, so nothing is pending after
	// the upgrade. An entry no binding can name, such as a literal `/*` on a policy
	// without wildcards, matches nothing either way.
	for _, raw := range t.Policy.Repositories {
		if e, ok := routing.ParseEntry(raw, t.Policy.Wildcards); ok {
			_ = s.Bind(e.Key(), t.OrganizationID, false)
		}
	}
	return nil
}

// legacyTeam is profile name's team as its routing state's one enrollment, nil when it
// has none or no organization is known for it. It carries the team's last validated
// policy under the organization and environment that fetched it, as today's rules apply
// it (config.Load); without one, an empty policy no refresh has validated, which blocks
// nothing and stays Expired until the relay's refresh validates it. A global policy
// collects nothing, as routing.State.ApplyRefresh has it for any team but personal, and a
// migrated team is never personal: the developer ticks personal themselves.
func (m Machine) legacyTeam(name string, p *config.Profile) (*routing.Team, error) {
	if p == nil || p.Team == "" {
		return nil, nil
	}
	cfg, err := config.Load(m.ConfigDir, m.StateDir, config.Overrides{Profile: name})
	if err != nil {
		return nil, err
	}
	pol := cfg.Policy
	validated := pol.TeamID == p.Team && !pol.FetchedAt.IsZero()
	if !validated {
		pol = config.Policy{Mode: config.ModeRepo}
	}
	org := cmp.Or(cfg.OrganizationID, pol.OrganizationID)
	if org == "" {
		return nil, nil
	}
	pol.TeamID, pol.OrganizationID, pol.AuthURL = p.Team, org, cfg.AuthURL
	if pol.Global() {
		pol.Mode, pol.Repositories = config.ModeRepo, nil
	}
	return &routing.Team{OrganizationID: org, AuthURL: cfg.AuthURL, TeamID: p.Team, Policy: pol}, nil
}

// preparing are the routing states under stateDir still in the Preparing phase. One that
// cannot be read is left out: it grants nothing, and is never rebuilt.
func preparing(stateDir string) ([]routing.State, error) {
	entries, err := os.ReadDir(filepath.Join(stateDir, routing.StatesDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []routing.State
	for _, e := range entries {
		profile, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !e.Type().IsRegular() {
			continue
		}
		s, ok, err := routing.Load(stateDir, profile)
		if err == nil && ok && s.Migration != nil && s.Migration.Phase == routing.Preparing {
			out = append(out, s)
		}
	}
	return out, nil
}

// newEnrollmentID names a new enrollment as routing.State.Select does.
func newEnrollmentID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
