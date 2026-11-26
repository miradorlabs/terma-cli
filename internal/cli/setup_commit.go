package cli

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/routing"
)

func (app *App) verifyActiveLane(account string) error {
	liveCfg, err := app.loadConfig()
	if err != nil {
		return err
	}
	if app.profileLane(liveCfg) != account {
		return fmt.Errorf("%w: active profile lane changed during setup", routing.ErrAccountMismatch)
	}
	return nil
}

// checkSetupBaseline verifies before mutating server-key storage that routing/<profile>.json
// still matches baseline (and for singleTeamOnly, has not been concurrently replaced).
func (app *App) checkSetupBaseline(profile, account string, b setupBaseline, singleTeamOnly bool) error {
	existing, found, loadErr := routing.Load(app.stateDir, profile)
	if loadErr != nil {
		if b.loadErr != nil {
			return nil
		}
		return loadErr
	}
	if !found {
		if b.found && b.loadErr == nil && b.sameAccount {
			return routing.ErrNoState
		}
		return nil
	}
	if existing.Account != account {
		if b.expectPrev != nil && existing.Account == b.expectPrev.Account && existing.StateID == b.expectPrev.StateID {
			return nil
		}
		return fmt.Errorf("%w: on disk %s, expected %s", routing.ErrAccountMismatch, existing.Account, account)
	}
	if singleTeamOnly && (b.expectPrev == nil || existing.StateID != b.expectPrev.StateID) {
		return routing.ErrStaleGeneration
	}
	return nil
}

// commitRoutingStateLocked writes targets into routing/<profile>.json via CAS while the
// caller exclusively holds the relay lock, and unticks the teams named in untick.
func (app *App) commitRoutingStateLocked(profile, account string, b setupBaseline, targets []routing.Team, untick map[string]bool, refreshes []setupRefresh, singleTeamOnly bool) (routing.State, bool, error) {
	now := time.Now().UTC()
	pre := make([]preFetchEntry, len(targets))
	for i, target := range targets {
		enrID, epoch, hadPre := preFetchTarget(b, target)
		pre[i] = preFetchEntry{enrID: enrID, epoch: epoch, hadPre: hadPre}
	}
	applyFresh := func(s *routing.State) error {
		if err := app.verifyActiveLane(account); err != nil {
			return err
		}
		for _, target := range targets {
			if _, err := s.Select(target); err != nil {
				return err
			}
		}
		return nil
	}
	for range 3 {
		if err := app.verifyActiveLane(account); err != nil {
			return routing.State{}, false, err
		}
		existing, found, loadErr := routing.Load(app.stateDir, profile)
		switch {
		case loadErr != nil:
			if b.loadErr == nil {
				return routing.State{}, false, loadErr
			}
			st, err := routing.ReplaceCorrupt(app.stateDir, profile, account, applyFresh)
			if errors.Is(err, routing.ErrStateExists) || errors.Is(err, routing.ErrNoState) {
				continue
			}
			return st, true, err
		case !found:
			if b.found && b.loadErr == nil && b.sameAccount {
				return routing.State{}, false, routing.ErrNoState
			}
			st, err := routing.Create(app.stateDir, profile, account, applyFresh)
			if errors.Is(err, routing.ErrStateExists) {
				continue
			}
			return st, false, err
		case existing.Account != account:
			if b.expectPrev == nil || existing.Account != b.expectPrev.Account || existing.StateID != b.expectPrev.StateID {
				return routing.State{}, false, fmt.Errorf("%w: on disk %s, expected %s", routing.ErrAccountMismatch, existing.Account, account)
			}
			st, err := routing.Replace(app.stateDir, profile, b.expectPrev, account, applyFresh)
			return st, true, err
		case singleTeamOnly && len(targets) == 1 && !sameSingleTeam(existing, targets[0]):
			if b.expectPrev == nil || existing.StateID != b.expectPrev.StateID {
				return routing.State{}, false, routing.ErrStaleGeneration
			}
			expectPrev := existing.Expect()
			st, err := routing.Replace(app.stateDir, profile, &expectPrev, account, applyFresh)
			if errors.Is(err, routing.ErrStaleGeneration) {
				continue
			}
			return st, false, err
		default:
			if singleTeamOnly && (b.expectPrev == nil || existing.StateID != b.expectPrev.StateID) {
				return routing.State{}, false, routing.ErrStaleGeneration
			}
			st, err := routing.Update(app.stateDir, profile, existing.ExpectConsent(), func(s *routing.State) error {
				if err := app.verifyActiveLane(account); err != nil {
					return err
				}
				for i, target := range targets {
					if err := upsertTeam(s, target, pre[i].enrID, pre[i].epoch, pre[i].hadPre, now); err != nil {
						return err
					}
				}
				for _, t := range slices.Clone(s.Teams) {
					if untick[t.TeamID] {
						s.Deselect(t.EnrollmentID)
					}
				}
				return applySetupRefreshes(s, refreshes)
			})
			if errors.Is(err, routing.ErrStaleGeneration) {
				continue
			}
			return st, false, err
		}
	}
	return routing.State{}, false, routing.ErrStaleGeneration
}

func preFetchTarget(b setupBaseline, target routing.Team) (string, int64, bool) {
	if !b.sameAccount {
		return "", 0, false
	}
	for _, t := range b.state.Teams {
		if t.TeamID == target.TeamID && t.OrganizationID == target.OrganizationID &&
			t.AuthURL == target.AuthURL && t.Personal == target.Personal {
			return t.EnrollmentID, t.Epoch, true
		}
	}
	return "", 0, false
}

func sameSingleTeam(st routing.State, target routing.Team) bool {
	return len(st.Teams) == 1 && st.Teams[0].TeamID == target.TeamID &&
		st.Teams[0].OrganizationID == target.OrganizationID && st.Teams[0].AuthURL == target.AuthURL
}

func upsertTeam(s *routing.State, target routing.Team, preEnrID string, preEpoch int64, hadPre bool, now time.Time) error {
	for _, cur := range s.Teams {
		if cur.TeamID == target.TeamID {
			if cur.OrganizationID == target.OrganizationID && cur.AuthURL == target.AuthURL && cur.Personal == target.Personal {
				if !hadPre || cur.EnrollmentID != preEnrID {
					return nil
				}
				pol := target.Policy
				_, err := s.ApplyRefresh(routing.Refresh{
					EnrollmentID: cur.EnrollmentID,
					Epoch:        preEpoch,
					Policy:       &pol,
					At:           now,
				})
				return err
			}
			s.Deselect(cur.EnrollmentID)
			break
		}
	}
	if target.Personal {
		for _, cur := range s.Teams {
			if cur.Personal {
				s.Deselect(cur.EnrollmentID)
				break
			}
		}
	}
	_, err := s.Select(target)
	return err
}
