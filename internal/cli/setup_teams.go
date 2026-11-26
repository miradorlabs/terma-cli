package cli

import (
	"cmp"
	"context"
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

type preFetchEntry struct {
	enrID  string
	epoch  int64
	hadPre bool
}

// setupTeams gates activation on a routing-capable relay, commits the chosen teams to
// routing/<profile>.json via CAS under the relay gate, persists committed
// policies/<team>.json files, updates profile metadata, and reports policy conflicts.
func (app *App) setupTeams(cmd *cobra.Command, cfg *config.Config, org *organization, disc *setupDiscovery, f setupFlags, ui *setupUI) (config.Policy, string, error) {
	ctx := cmd.Context()
	if err := app.gateRoutingActivation(); err != nil {
		return config.Policy{}, "", err
	}
	account := cfg.Account
	if !routing.ValidAccount(account) {
		return config.Policy{}, "", fmt.Errorf("invalid account %q for routing state", account)
	}
	if disc != nil && !disc.stub {
		return app.commitDiscoveredTeams(ctx, cfg, account, disc, ui)
	}
	// Single-target path for --desktop-grant and TERMA_POLICY_STUB.
	baseline := app.loadSetupBaseline(cfg.ProfileName, account)
	if disc != nil {
		baseline = disc.baseline
		if org == nil && len(disc.orgs) == 1 {
			o := disc.orgs[0]
			org = &o
		}
	}
	target, teamName, err := app.fetchSetupTarget(cmd, cfg, org, account, f.desktopGrant)
	if err != nil {
		if baseline.sameAccount && target.TeamID != "" && policy.Classify(err) == routing.OrgAccessLost {
			app.commitTargetOrgAccessLost(ctx, cfg, account, baseline, target)
		}
		return config.Policy{}, "", err
	}
	orgLabel := cmp.Or(cfg.OrganizationName, cfg.OrganizationID)
	if org != nil && org.Name != "" {
		orgLabel = org.Name
	}

	var refreshes []setupRefresh
	if baseline.sameAccount {
		refreshes = app.refreshExistingTeams(ctx, cfg, account, baseline.state.Teams, map[string]bool{target.TeamID: true}, nil)
	}
	var committed routing.State
	var accountSwitched bool
	err = daemon.WithRoutingRelayGate(app.stateDir, app.version, func() error {
		var cerr error
		committed, accountSwitched, cerr = app.commitRoutingStateLocked(cfg.ProfileName, account, baseline, []routing.Team{target}, nil, refreshes, false)
		return cerr
	})
	if err != nil {
		return config.Policy{}, "", err
	}
	primary := committedPrimary(committed, target)
	if primary.Personal {
		teamName = personalLabel
	}
	if err := app.persistCommittedPolicies(committed); err != nil {
		return config.Policy{}, "", err
	}
	if err := app.recordSetupTeam(cfg, account, primary.OrganizationID, orgLabel, primary.TeamID, teamName); err != nil {
		return config.Policy{}, "", err
	}
	if accountSwitched || cfg.Policy.OrganizationID != primary.OrganizationID || cfg.Policy.AuthURL != primary.AuthURL {
		if dir, derr := daemon.Dir(app.stateDir); derr == nil {
			daemon.Stop(dir)
		}
	}
	ui.policyFetched(teamName, primary.Policy)
	ui.reportConflicts(committed, loadTeamNames(app.stateDir, cfg.ProfileName))
	return primary.Policy, teamName, nil
}

func (app *App) commitDiscoveredTeams(ctx context.Context, cfg *config.Config, account string, disc *setupDiscovery, ui *setupUI) (config.Policy, string, error) {
	baseline := disc.baseline

	targetTeams := make([]routing.Team, 0, len(disc.teams))
	skipTeams := make(map[string]bool, len(disc.teams))
	for _, dt := range disc.teams {
		targetTeams = append(targetTeams, dt.team)
		skipTeams[dt.team.TeamID] = true
	}

	var refreshes []setupRefresh
	if baseline.sameAccount {
		now := time.Now().UTC()
		inaccessibleSet := map[string]bool{}
		for _, orgID := range disc.inaccessibleOrgs {
			inaccessibleSet[orgID] = true
			if epochs := orgEpochSnapshot(baseline.state.Teams, orgID, cfg.AuthURL); len(epochs) > 0 {
				refreshes = append(refreshes, setupRefresh{
					refresh:   routing.Refresh{Failure: routing.OrgAccessLost, At: now},
					orgID:     orgID,
					authURL:   cfg.AuthURL,
					orgEpochs: epochs,
				})
			}
		}
		refreshes = append(refreshes, app.refreshExistingTeams(ctx, cfg, account, baseline.state.Teams, skipTeams, inaccessibleSet)...)
	}

	var committed routing.State
	var accountSwitched bool
	err := daemon.WithRoutingRelayGate(app.stateDir, app.version, func() error {
		var cerr error
		committed, accountSwitched, cerr = app.commitRoutingStateLocked(cfg.ProfileName, account, baseline, targetTeams, disc.untick, refreshes, false)
		return cerr
	})
	if err != nil {
		return config.Policy{}, "", err
	}
	if err := app.persistCommittedPolicies(committed); err != nil {
		return config.Policy{}, "", err
	}

	primaryDisc := disc.teams[0]
	if cfg.Team != "" {
		for _, dt := range disc.teams {
			if dt.team.TeamID == cfg.Team {
				primaryDisc = dt
				break
			}
		}
	}
	primary := committedPrimary(committed, primaryDisc.team)
	if primary.Personal {
		primaryDisc.teamName = personalLabel
	}
	if err := app.recordSetupTeam(cfg, account, primary.OrganizationID, primaryDisc.orgName, primary.TeamID, primaryDisc.teamName); err != nil {
		return config.Policy{}, "", err
	}
	if accountSwitched || cfg.Policy.OrganizationID != primary.OrganizationID || cfg.Policy.AuthURL != primary.AuthURL {
		if dir, derr := daemon.Dir(app.stateDir); derr == nil {
			daemon.Stop(dir)
		}
	}

	if len(disc.teams) == 1 && len(disc.orgs) == 1 {
		ui.policyFetched(primaryDisc.teamName, primary.Policy)
	} else {
		ui.teamsConfigured(disc.orgs, disc.teams)
	}
	for _, w := range disc.warnings {
		ui.Caution("Teams", w)
	}
	ui.reportConflicts(committed, disc.names)
	return primary.Policy, primaryDisc.teamName, nil
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

func (app *App) fetchSetupTarget(cmd *cobra.Command, cfg *config.Config, org *organization, account string, desktopGrant bool) (routing.Team, string, error) {
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
	teamName, err := app.selectPolicyTeam(cmd, cfg, false)
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

func (app *App) refreshExistingTeams(ctx context.Context, cfg *config.Config, account string, teams []routing.Team, skipTeams, skipOrgs map[string]bool) []setupRefresh {
	var out []setupRefresh
	for _, t := range teams {
		if skipTeams[t.TeamID] || skipOrgs[t.OrganizationID] || t.AuthURL != cfg.AuthURL {
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
// returned 403 ORG_NOT_ACCESSIBLE and still refreshes sibling enrollments in other orgs.
func (app *App) commitTargetOrgAccessLost(ctx context.Context, cfg *config.Config, account string, b setupBaseline, target routing.Team) {
	app.commitInaccessibleOrgs(ctx, cfg, account, b, []string{target.OrganizationID})
}

func (app *App) commitInaccessibleOrgs(ctx context.Context, cfg *config.Config, account string, b setupBaseline, orgIDs []string) {
	skipOrgs := map[string]bool{}
	now := time.Now().UTC()
	var refreshes []setupRefresh
	for _, orgID := range orgIDs {
		skipOrgs[orgID] = true
		if epochs := orgEpochSnapshot(b.state.Teams, orgID, cfg.AuthURL); len(epochs) > 0 {
			refreshes = append(refreshes, setupRefresh{
				refresh:   routing.Refresh{Failure: routing.OrgAccessLost, At: now},
				orgID:     orgID,
				authURL:   cfg.AuthURL,
				orgEpochs: epochs,
			})
		}
	}
	refreshes = append(refreshes, app.refreshExistingTeams(ctx, cfg, account, b.state.Teams, nil, skipOrgs)...)
	if len(refreshes) == 0 {
		return
	}
	var committed routing.State
	err := daemon.WithRoutingRelayGate(app.stateDir, app.version, func() error {
		st, uerr := routing.Update(app.stateDir, cfg.ProfileName, routing.Expect{Account: account}, func(s *routing.State) error {
			if err := app.verifyActiveLane(account); err != nil {
				return err
			}
			return applySetupRefreshes(s, refreshes)
		})
		committed = st
		return uerr
	})
	if err == nil {
		_ = app.persistCommittedPolicies(committed)
	}
}
