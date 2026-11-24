package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/policy"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

type setupBaseline struct {
	found       bool
	loadErr     error
	sameAccount bool
	state       routing.State
	// expectPrev pins Account + StateID for an intentional cross-account Replace (or
	// single-team replacement) so an in-flight background refresh on the prior state
	// does not block cutover while any concurrent replacement or ABA reuse is rejected.
	expectPrev *routing.Expect
}

type setupRefresh struct {
	refresh   routing.Refresh
	orgID     string
	authURL   string
	orgEpochs map[string]int64
}

// setupTeams gates activation on a routing-capable relay, snapshots pre-fetch routing
// state, fetches the chosen team's policy (plus refreshes sibling enrollments of the same
// account for sign-in recovery), commits routing/<profile>.json via CAS under the relay
// gate, persists committed policies/<team>.json files, and updates profile metadata.
func (app *App) setupTeams(cmd *cobra.Command, cfg *config.Config, org *organization, f setupFlags, ui *setupUI) (config.Policy, string, error) {
	ctx := cmd.Context()
	if err := app.gateRoutingActivation(); err != nil {
		return config.Policy{}, "", err
	}
	account := cfg.Account
	if !routing.ValidAccount(account) {
		return config.Policy{}, "", fmt.Errorf("invalid account %q for routing state", account)
	}
	// Snapshot pre-setup state BEFORE any network fetch so target epoch fencing, sibling
	// OrgAccessLost epochs, and cross-account Replace CAS all anchor to pre-fetch truth.
	baseline := app.loadSetupBaseline(cfg.ProfileName, account)
	target, teamName, err := app.fetchSetupTarget(cmd, cfg, org, account, f.desktopGrant, !f.assumeYes && canPrompt())
	if err != nil {
		if baseline.sameAccount && target.TeamID != "" && policy.Classify(err) == routing.OrgAccessLost {
			app.commitTargetOrgAccessLost(ctx, cfg, account, baseline, target)
		}
		return config.Policy{}, "", err
	}
	var existingBindings map[string]routing.Binding
	if baseline.sameAccount {
		existingBindings = baseline.state.Bindings
	}
	orgLabel := cmp.Or(cfg.OrganizationName, cfg.OrganizationID)
	if org != nil && org.Name != "" {
		orgLabel = org.Name
	}
	bindings, err := decideSetupBindings(target, orgLabel, existingBindings, !f.assumeYes && canPrompt())
	if err != nil {
		return config.Policy{}, "", err
	}
	// Sign-in recovery: refresh all other enrollments of the same account before taking the lock.
	var refreshes []setupRefresh
	if baseline.sameAccount {
		refreshes = app.refreshExistingTeams(ctx, cfg, account, baseline.state.Teams, target.TeamID)
	}
	var committed routing.State
	var accountSwitched bool
	err = daemon.WithRoutingRelayGate(app.stateDir, app.version, func() error {
		var cerr error
		committed, accountSwitched, cerr = app.commitRoutingStateLocked(cfg.ProfileName, account, baseline, target, refreshes, bindings, false)
		return cerr
	})
	if err != nil {
		return config.Policy{}, "", err
	}
	primary := committedPrimary(committed, target)
	if err := app.persistCommittedPolicies(committed); err != nil {
		return config.Policy{}, "", err
	}
	if err := app.recordSetupTeam(cfg, account, primary.TeamID, teamName); err != nil {
		return config.Policy{}, "", err
	}
	if accountSwitched || cfg.Policy.OrganizationID != primary.OrganizationID || cfg.Policy.AuthURL != primary.AuthURL {
		if dir, derr := daemon.Dir(app.stateDir); derr == nil {
			daemon.Stop(dir)
		}
	}
	ui.policyFetched(teamName, primary.Policy)
	return primary.Policy, teamName, nil
}

// gateRoutingActivation supersedes any running pre-routing relay and refuses activation if
// a pre-routing relay still holds the lock, then purges leftover pre-cutover outbox parts
// when no relay is running.
func (app *App) gateRoutingActivation() error {
	if err := daemon.EnsureRoutingRelay(app.stateDir, app.version); err != nil {
		return err
	}
	if app.stateDir != "" && !daemon.Running(claim.Dir(app.stateDir)) {
		_, _ = relay.PurgeOldOutbox(filepath.Join(claim.Dir(app.stateDir), relay.OutboxDir), time.Now())
	}
	return nil
}

func (app *App) loadSetupBaseline(profile, account string) setupBaseline {
	st, found, err := routing.Load(app.stateDir, profile)
	b := setupBaseline{found: found, loadErr: err}
	if found && err == nil {
		b.state = st
		b.sameAccount = st.Account == account
		b.expectPrev = &routing.Expect{Account: st.Account, StateID: st.StateID}
	}
	return b
}

func (app *App) fetchSetupTarget(cmd *cobra.Command, cfg *config.Config, org *organization, account string, desktopGrant, ask bool) (routing.Team, string, error) {
	ctx := cmd.Context()
	personal := org != nil && strings.EqualFold(org.Type, "personal")
	if desktopGrant && personal {
		return routing.Team{}, "", &auth.GrantError{
			Code:    auth.GrantBadInput,
			Message: "Personal cannot be enabled via --desktop-grant; run `terma setup` interactively",
		}
	}
	if config.PolicyStub() != "" {
		pol, err := app.policies().Fetch(ctx, cfg)
		if err != nil {
			return routing.Team{}, "", err
		}
		teamID := cmp.Or(cfg.ProjectID, cfg.Team, pol.TeamID, pol.DefaultProjectID, "stub")
		orgID := cmp.Or(cfg.OrganizationID, pol.OrganizationID, "org_stub")
		authURL := cmp.Or(cfg.AuthURL, pol.AuthURL)
		if pol.Global() && !personal {
			if desktopGrant {
				pol.Mode, pol.Repositories = config.ModeRepo, nil
			} else {
				personal = true
				orgID = "org_personal_stub"
			}
		}
		pol.TeamID, pol.OrganizationID, pol.AuthURL = teamID, orgID, authURL
		return routing.Team{
			OrganizationID: orgID,
			AuthURL:        authURL,
			TeamID:         teamID,
			Personal:       personal,
			Policy:         pol,
		}, "", nil
	}
	teamName, err := app.selectPolicyTeam(cmd, cfg, ask)
	if err != nil {
		return routing.Team{}, "", err
	}
	target := routing.Team{
		OrganizationID: cfg.OrganizationID,
		AuthURL:        cfg.AuthURL,
		TeamID:         cfg.ProjectID,
		Personal:       personal,
	}
	pol, err := app.policies().FetchTeam(ctx, cfg, account, target)
	if err != nil {
		return target, teamName, err
	}
	if !personal && pol.Global() {
		pol.Mode, pol.Repositories = config.ModeRepo, nil
	}
	target.Policy = pol
	return target, teamName, nil
}

func orgEpochSnapshot(teams []routing.Team, orgID, authURL string) map[string]int64 {
	epochs := make(map[string]int64)
	for _, sib := range teams {
		if sib.OrganizationID == orgID && sib.AuthURL == authURL {
			epochs[sib.EnrollmentID] = sib.Epoch
		}
	}
	return epochs
}

func (app *App) refreshExistingTeams(ctx context.Context, cfg *config.Config, account string, teams []routing.Team, skipTeamID string) []setupRefresh {
	var out []setupRefresh
	for _, t := range teams {
		if t.TeamID == skipTeamID || t.AuthURL != cfg.AuthURL {
			continue
		}
		orgEpochs := orgEpochSnapshot(teams, t.OrganizationID, t.AuthURL)
		ref, _ := app.policies().RefreshTeam(ctx, cfg, account, t)
		out = append(out, setupRefresh{
			refresh:   ref,
			orgID:     t.OrganizationID,
			authURL:   t.AuthURL,
			orgEpochs: orgEpochs,
		})
	}
	return out
}

func applySetupRefreshes(s *routing.State, refreshes []setupRefresh) error {
	for _, r := range refreshes {
		if r.refresh.Failure == routing.OrgAccessLost {
			s.ApplyOrgAccessLost(r.orgID, r.authURL, r.orgEpochs, r.refresh.At)
			continue
		}
		if _, err := s.ApplyRefresh(r.refresh); err != nil {
			return err
		}
	}
	return nil
}

// commitTargetOrgAccessLost records an org-wide OrgAccessLost for a target fetch that
// returned 403 ORG_NOT_ACCESSIBLE and still refreshes sibling enrollments in other orgs
// so sign-in recovery does not leave Active grants in the lost org or SignInNeeded elsewhere.
func (app *App) commitTargetOrgAccessLost(ctx context.Context, cfg *config.Config, account string, b setupBaseline, target routing.Team) {
	orgEpochs := orgEpochSnapshot(b.state.Teams, target.OrganizationID, target.AuthURL)
	refreshes := app.refreshExistingTeams(ctx, cfg, account, b.state.Teams, target.TeamID)
	if len(orgEpochs) == 0 && len(refreshes) == 0 {
		return
	}
	now := time.Now().UTC()
	var committed routing.State
	err := daemon.WithRoutingRelayGate(app.stateDir, app.version, func() error {
		st, uerr := routing.Update(app.stateDir, cfg.ProfileName, routing.Expect{Account: account}, func(s *routing.State) error {
			if err := app.verifyActiveLane(account); err != nil {
				return err
			}
			s.ApplyOrgAccessLost(target.OrganizationID, target.AuthURL, orgEpochs, now)
			return applySetupRefreshes(s, refreshes)
		})
		committed = st
		return uerr
	})
	if err == nil {
		_ = app.persistCommittedPolicies(committed)
	}
}

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

// commitRoutingStateLocked writes target into routing/<profile>.json via CAS while the
// caller exclusively holds the relay lock:
//   - Verifies the live profile lane under routing/<profile>.lock before every write;
//   - ReplaceCorrupt only when baseline observed a corrupt file;
//   - Create when absent (failing closed with ErrNoState if same-account state was torn down);
//   - Replace(b.expectPrev) ONLY when the prior state on disk is the exact baseline StateID;
//   - Update(existing.ExpectConsent()) with epoch-fenced target upsert and org-wide recovery.
func (app *App) commitRoutingStateLocked(profile, account string, b setupBaseline, target routing.Team, refreshes []setupRefresh, bindings []bindingChoice, singleTeamOnly bool) (routing.State, bool, error) {
	now := time.Now().UTC()
	preEnrID, preEpoch, hadPre := preFetchTarget(b, target)
	applyFresh := func(s *routing.State) error {
		if err := app.verifyActiveLane(account); err != nil {
			return err
		}
		if _, err := s.Select(target); err != nil {
			return err
		}
		return applySetupBindings(s, target, bindings)
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
		case singleTeamOnly && !sameSingleTeam(existing, target):
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
				if err := upsertTeam(s, target, preEnrID, preEpoch, hadPre, now); err != nil {
					return err
				}
				if err := applySetupRefreshes(s, refreshes); err != nil {
					return err
				}
				return applySetupBindings(s, target, bindings)
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
