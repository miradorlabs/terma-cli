package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// MintLockFile is the lock, in the config directory, that serializes creating team keys on
// the machine, so the backend's reuse of a key by its name finds this machine's and no
// other's: whatever creates one takes it (MintTeamKey).
const MintLockFile = "key-mint"

// mintWait bounds the wait for another process's creation, which is one request.
const mintWait = 45 * time.Second

// CreateTeamKey mints a team's key on the backend, with its organization's sign-in, named
// for this installation and the team's enrollment.
type CreateTeamKey func(ctx context.Context, cfg *config.Config, team routing.Team) (string, error)

// MintTeamKey gives team's enrollment a key unless it has one: created with create, filed
// in keys.v2.json under the enrollment with the hosts of cfg's environment, all under
// MintLockFile. A key the keychain will not give up now is never minted over.
func MintTeamKey(ctx context.Context, cfg *config.Config, team routing.Team, create CreateTeamKey) error {
	if team.EnrollmentID == "" || team.TeamID == "" {
		return errors.New("a team key is minted only for a selected team")
	}
	return flock.Locked(filepath.Join(cfg.Dir, MintLockFile), mintWait, func() error {
		if key, _, err := keystore.GetTeam(cfg.Dir, team.EnrollmentID); err != nil || key != "" {
			return err
		}
		key, err := create(ctx, cfg, team)
		if err != nil {
			return err
		}
		if key == "" {
			return errors.New("the server minted a key but returned no value for it")
		}
		return keystore.SetTeam(cfg.Dir, team.EnrollmentID, team.TeamID, key, keystore.HostsOf(cfg))
	})
}

// TeamKeyMinter mints, off the export path, the keys of selected teams this machine holds
// none for, while their parts wait, as KeyMinter does a claimed project's.
type TeamKeyMinter struct {
	ctx    context.Context
	cfg    *config.Config
	create CreateTeamKey

	mu       sync.Mutex
	inFlight map[string]bool
	failedAt map[string]time.Time
}

// NewTeamKeyMinter mints with create until ctx ends.
func NewTeamKeyMinter(ctx context.Context, cfg *config.Config, create CreateTeamKey) *TeamKeyMinter {
	return &TeamKeyMinter{ctx: ctx, cfg: cfg, create: create, inFlight: map[string]bool{}, failedAt: map[string]time.Time{}}
}

// Mint starts minting team's key unless one is on the way, or failed lately. A profile set
// up with a server key, or under TERMA_API_KEY, mints none: a server key cannot create
// another.
func (m *TeamKeyMinter) Mint(team routing.Team) {
	if m == nil || m.create == nil || m.cfg.ServerKeySignIn || m.cfg.APIKey != "" || team.EnrollmentID == "" {
		return
	}
	id := team.EnrollmentID
	m.mu.Lock()
	if m.inFlight[id] || time.Since(m.failedAt[id]) < KeyMintBackoff {
		m.mu.Unlock()
		return
	}
	m.inFlight[id] = true
	m.mu.Unlock()
	go func() {
		ok := m.mintNow(team)
		m.mu.Lock()
		delete(m.inFlight, id)
		if ok {
			delete(m.failedAt, id)
		} else {
			m.failedAt[id] = time.Now()
		}
		m.mu.Unlock()
	}()
}

// mintNow mints team's key, if the routing state still lets it have one: selected, and
// neither Suspended nor Refused. A key that lands after the team was refused or ended
// meanwhile is taken back, so a refusal is never undone by a creation already in flight.
func (m *TeamKeyMinter) mintNow(team routing.Team) bool {
	if !keyAllowed(m.cfg, team.EnrollmentID) {
		return false
	}
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	if err := MintTeamKey(ctx, m.cfg, team, m.create); err != nil {
		return false
	}
	if !keyAllowed(m.cfg, team.EnrollmentID) {
		_ = keystore.DeleteTeam(m.cfg.Dir, team.EnrollmentID)
		return false
	}
	return true
}

// keyAllowed reports whether enrollment may hold a key now: selected in the profile's
// committed routing state, and neither Suspended nor Refused.
func keyAllowed(cfg *config.Config, enrollment string) bool {
	st, ok, err := routing.Load(cfg.StateDir, cfg.ProfileName)
	if err != nil || !ok || st.Migration != nil && st.Migration.Phase != routing.Committed {
		return false
	}
	t := team(st, enrollment)
	if t == nil {
		return false
	}
	s := t.StateAt(time.Now())
	return s != routing.Suspended && s != routing.Refused
}
