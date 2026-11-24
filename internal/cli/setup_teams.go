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

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

type bindingChoice struct {
	key    string
	org    string
	refuse bool
}

// setupTeams gates activation on a routing-capable relay, fetches the chosen team's policy
// (plus refreshes all existing same-account enrollments for sign-in recovery), commits
// routing/<profile>.json via CAS (Create, Update, or Replace on account switch), persists
// committed policies/<team>.json files, and updates profile display metadata.
func (app *App) setupTeams(cmd *cobra.Command, cfg *config.Config, org *organization, f setupFlags, ui *setupUI) (config.Policy, string, error) {
	ctx := cmd.Context()
	if err := app.gateRoutingActivation(); err != nil {
		return config.Policy{}, "", err
	}
	account := cfg.Account
	if !routing.ValidAccount(account) {
		return config.Policy{}, "", fmt.Errorf("invalid account %q for routing state", account)
	}
	target, teamName, err := app.fetchSetupTarget(cmd, cfg, org, account, !f.assumeYes && canPrompt())
	if err != nil {
		return config.Policy{}, "", err
	}
	// Load existing state to plan bindings and sign-in recovery refreshes outside the lock.
	existing, found, loadErr := routing.Load(app.stateDir, cfg.ProfileName)
	var existingBindings map[string]routing.Binding
	if found && loadErr == nil && existing.Account == account {
		existingBindings = existing.Bindings
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
	var refreshes []routing.Refresh
	if found && loadErr == nil && existing.Account == account {
		refreshes = app.refreshExistingTeams(ctx, cfg, account, existing.Teams, target.TeamID)
	}
	committed, accountSwitched, err := app.commitRoutingState(cfg.ProfileName, account, target, refreshes, bindings, false)
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

func (app *App) fetchSetupTarget(cmd *cobra.Command, cfg *config.Config, org *organization, account string, ask bool) (routing.Team, string, error) {
	ctx := cmd.Context()
	if config.PolicyStub() != "" {
		pol, err := app.policies().Fetch(ctx, cfg)
		if err != nil {
			return routing.Team{}, "", err
		}
		teamID := cmp.Or(cfg.ProjectID, cfg.Team, pol.TeamID, pol.DefaultProjectID, "stub")
		orgID := cmp.Or(cfg.OrganizationID, pol.OrganizationID, "org_stub")
		authURL := cmp.Or(cfg.AuthURL, pol.AuthURL)
		personal := org != nil && org.Type == "personal"
		if pol.Global() && !personal {
			personal = true
			orgID = "org_personal_stub"
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
	personal := org != nil && org.Type == "personal"
	target := routing.Team{
		OrganizationID: cfg.OrganizationID,
		AuthURL:        cfg.AuthURL,
		TeamID:         cfg.ProjectID,
		Personal:       personal,
	}
	pol, err := app.policies().FetchTeam(ctx, cfg, account, target)
	if err != nil {
		return routing.Team{}, "", err
	}
	if !personal && pol.Global() {
		pol.Mode, pol.Repositories = config.ModeRepo, nil
	}
	target.Policy = pol
	return target, teamName, nil
}

func (app *App) refreshExistingTeams(ctx context.Context, cfg *config.Config, account string, teams []routing.Team, skipTeamID string) []routing.Refresh {
	var out []routing.Refresh
	for _, t := range teams {
		if t.TeamID == skipTeamID || t.AuthURL != cfg.AuthURL {
			continue
		}
		ref, _ := app.policies().RefreshTeam(ctx, cfg, account, t)
		out = append(out, ref)
	}
	return out
}

// commitRoutingState writes target into routing/<profile>.json via CAS:
//   - Create when absent;
//   - Replace(nil) when corrupt/unreadable on disk;
//   - Replace(&existing.Expect()) when switching accounts (alice -> bob) or when
//     singleTeamOnly is set and the existing state holds a different team set (server-key);
//   - Update(existing.ExpectConsent()) when preserving existing same-account enrollments.
func (app *App) commitRoutingState(profile, account string, target routing.Team, refreshes []routing.Refresh, bindings []bindingChoice, singleTeamOnly bool) (routing.State, bool, error) {
	now := time.Now().UTC()
	applyFresh := func(s *routing.State) error {
		if _, err := s.Select(target); err != nil {
			return err
		}
		return applySetupBindings(s, target, bindings)
	}
	for range 3 {
		existing, found, loadErr := routing.Load(app.stateDir, profile)
		switch {
		case loadErr != nil:
			st, err := routing.Replace(app.stateDir, profile, nil, account, applyFresh)
			return st, true, err
		case !found:
			st, err := routing.Create(app.stateDir, profile, account, applyFresh)
			if errors.Is(err, routing.ErrStateExists) {
				continue
			}
			return st, false, err
		case existing.Account != account || (singleTeamOnly && !sameSingleTeam(existing, target)):
			expectPrev := existing.Expect()
			st, err := routing.Replace(app.stateDir, profile, &expectPrev, account, applyFresh)
			if errors.Is(err, routing.ErrStaleGeneration) {
				continue
			}
			return st, existing.Account != account, err
		default:
			st, err := routing.Update(app.stateDir, profile, existing.ExpectConsent(), func(s *routing.State) error {
				for _, r := range refreshes {
					if _, rerr := s.ApplyRefresh(r); rerr != nil {
						return rerr
					}
				}
				if err := upsertTeam(s, target, now); err != nil {
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

func sameSingleTeam(st routing.State, target routing.Team) bool {
	return len(st.Teams) == 1 && st.Teams[0].TeamID == target.TeamID &&
		st.Teams[0].OrganizationID == target.OrganizationID && st.Teams[0].AuthURL == target.AuthURL
}

func upsertTeam(s *routing.State, target routing.Team, now time.Time) error {
	for _, cur := range s.Teams {
		if cur.TeamID == target.TeamID {
			if cur.OrganizationID == target.OrganizationID && cur.AuthURL == target.AuthURL && cur.Personal == target.Personal {
				pol := target.Policy
				_, err := s.ApplyRefresh(routing.Refresh{
					EnrollmentID: cur.EnrollmentID,
					Epoch:        cur.Epoch,
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

func committedPrimary(st routing.State, fallback routing.Team) routing.Team {
	for _, t := range st.Teams {
		if t.TeamID == fallback.TeamID {
			return t
		}
	}
	return fallback
}

func (app *App) persistCommittedPolicies(st routing.State) error {
	for _, t := range st.Teams {
		if t.Policy.FetchedAt.IsZero() {
			continue
		}
		if err := routing.StoreTeamPolicy(app.stateDir, t); err != nil {
			return err
		}
	}
	return nil
}

func (app *App) recordSetupTeam(cfg *config.Config, account, teamID, teamName string) error {
	var rejected error
	err := config.UpdateProfile(app.dir, cfg.ProfileName, func(p *config.Profile) {
		if account != sendas.NoPersonLane && p.Account != account {
			rejected = errors.New("signed in as someone else while setting up teams")
			return
		}
		if p.Team != teamID {
			p.TeamName = ""
		}
		p.Team = teamID
		if teamName != "" && teamName != teamID {
			p.TeamName = teamName
		}
	})
	if err != nil {
		return err
	}
	return rejected
}

// decideSetupBindings computes the binding decisions for target's listed repositories.
// Personal and global teams need no bindings; entries already covered in existing are
// skipped so existing developer decisions are never overwritten. Under non-interactive or
// -y mode (ask == false), safe defaults bind each needed entry key (exact repo for exact
// entries, owner for wildcards) to target.OrganizationID.
func decideSetupBindings(target routing.Team, orgLabel string, existing map[string]routing.Binding, ask bool) ([]bindingChoice, error) {
	if target.Personal || target.Policy.Global() || len(target.Policy.Repositories) == 0 {
		return nil, nil
	}
	var needed []routing.Entry
	seen := map[string]bool{}
	var owners []string
	seenOwner := map[string]bool{}
	anyWildcard := false
	for _, raw := range target.Policy.Repositories {
		e, ok := routing.ParseEntry(raw, target.Policy.Wildcards)
		if !ok || alreadyDecided(existing, e) || seen[e.Key()] {
			continue
		}
		seen[e.Key()] = true
		needed = append(needed, e)
		if e.Wildcard() {
			anyWildcard = true
		}
		owner := routing.Owner(e.Key())
		if !seenOwner[owner] {
			seenOwner[owner] = true
			owners = append(owners, owner)
		}
	}
	if len(needed) == 0 {
		return nil, nil
	}
	if !ask {
		out := make([]bindingChoice, 0, len(needed))
		for _, e := range needed {
			out = append(out, bindingChoice{key: e.Key(), org: target.OrganizationID})
		}
		return out, nil
	}
	if !anyWildcard {
		items := []prompt.Item{
			{Label: fmt.Sprintf("Bind just these %d repositor%s to %s", len(needed), pluralY(len(needed)), orgLabel), Selected: true},
			{Label: fmt.Sprintf("Bind whole owner (%s) to %s", strings.Join(owners, ", "), orgLabel)},
			{Label: "Decide later (hold matching sessions until bound)"},
		}
		choice, err := prompt.Choose("Bind repositories to "+orgLabel+"?", items)
		if errors.Is(err, prompt.ErrCancelled) {
			return nil, errCancelled
		}
		if err != nil {
			return nil, err
		}
		switch choice {
		case 0:
			out := make([]bindingChoice, 0, len(needed))
			for _, e := range needed {
				out = append(out, bindingChoice{key: e.Key(), org: target.OrganizationID})
			}
			return out, nil
		case 1:
			out := make([]bindingChoice, 0, len(owners))
			for _, o := range owners {
				out = append(out, bindingChoice{key: o, org: target.OrganizationID})
			}
			return out, nil
		default:
			return nil, nil
		}
	}
	items := []prompt.Item{
		{Label: fmt.Sprintf("Bind %s to %s", strings.Join(owners, ", "), orgLabel), Selected: true},
		{Label: "Refuse (do not collect these repositories for any organization)"},
		{Label: "Decide later (hold matching sessions until bound)"},
	}
	choice, err := prompt.Choose("Bind repositories to "+orgLabel+"?", items)
	if errors.Is(err, prompt.ErrCancelled) {
		return nil, errCancelled
	}
	if err != nil {
		return nil, err
	}
	switch choice {
	case 0:
		out := make([]bindingChoice, 0, len(needed))
		for _, e := range needed {
			out = append(out, bindingChoice{key: e.Key(), org: target.OrganizationID})
		}
		return out, nil
	case 1:
		out := make([]bindingChoice, 0, len(needed))
		for _, e := range needed {
			out = append(out, bindingChoice{key: e.Key(), refuse: true})
		}
		return out, nil
	default:
		return nil, nil
	}
}

func applySetupBindings(s *routing.State, target routing.Team, choices []bindingChoice) error {
	if target.Personal || len(choices) == 0 {
		return nil
	}
	for _, c := range choices {
		if _, decided := s.Bindings[c.key]; decided {
			continue
		}
		if config.IsRepositoryKey(c.key) {
			if _, ownerDecided := s.Bindings[routing.Owner(c.key)]; ownerDecided {
				continue
			}
		}
		if c.refuse {
			if err := s.Refuse(c.key); err != nil {
				return err
			}
		} else if err := s.Bind(c.key, c.org, false); err != nil {
			return err
		}
	}
	return nil
}

func alreadyDecided(bindings map[string]routing.Binding, e routing.Entry) bool {
	if len(bindings) == 0 {
		return false
	}
	if _, ok := bindings[e.Key()]; ok {
		return true
	}
	if !e.Wildcard() {
		_, ok := bindings[routing.Owner(e.Key())]
		return ok
	}
	return false
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
