package cli

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

func (app *App) newRouteCommand() *cobra.Command {
	route := &cobra.Command{
		Use:    "route",
		Short:  "Explain where terma sends what agents do in a folder",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	route.AddCommand(&cobra.Command{
		Use:   "explain [path]",
		Short: "Say which team a session started in a folder goes to, and why",
		Long: `Says which ticked team a session started in path (default: here) would go to: the
policy entry that matched, the binding that allowed or blocked it, the teams passed
over and why, and whether a record whose location is unknown may go to personal.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "."
			if len(args) == 1 {
				path = args[0]
			}
			return app.runRouteExplain(cmd, path)
		},
	})
	return route
}

func (app *App) runRouteExplain(cmd *cobra.Command, path string) error {
	cfg, err := app.loadConfig()
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a folder", path)
	}
	loc := locationOf(cmd, abs)
	out := style.Highlight(cmd.OutOrStdout())
	if !config.Migrated(app.dir) {
		explainLegacy(out, cfg, loc)
		return nil
	}
	st, err := loadRoutingState(app.stateDir, cfg.ProfileName)
	if err != nil {
		return err
	}
	explainRoute(out, st, loadTeamNames(app.stateDir, cfg.ProfileName), loc, time.Now())
	return nil
}

// locationOf is where a session started in dir runs, as a hook sees it: the checkout's
// hosted origin, else outside git.
func locationOf(cmd *cobra.Command, dir string) routing.Location {
	if _, gitDir, err := termaproject.Locate(cmd.Context(), dir); err == nil && gitDir != "" {
		if origin := gitx.RepositoryFS(gitDir); origin != "" {
			return routing.Location{Kind: routing.Repository, Repository: origin}
		}
	}
	return routing.Location{Kind: routing.OutsideGit}
}

// explainLegacy answers for a machine not yet routing by teams: its one team's policy.
func explainLegacy(w io.Writer, cfg *config.Config, loc routing.Location) {
	fmt.Fprintf(w, "Location:  %s\n", locationWords(loc))
	pol := cfg.Policy.InForce(cfg.OrganizationID, cfg.AuthURL)
	collected := pol.Validated() && !pol.Expired(time.Now()) &&
		(pol.Global() || loc.Kind == routing.Repository && pol.Admits(config.Repository{Origin: loc.Repository}))
	team := cmp.Or(cfg.TeamName, cfg.Team, "your team")
	if collected {
		fmt.Fprintf(w, "Goes to:   %s, the one team this machine collects for\n", team)
	} else {
		fmt.Fprintf(w, "Goes to:   nothing leaves: %s's policy does not collect it\n", team)
	}
	fmt.Fprintln(w, "This machine routes by its one team until `terma teams` moves it to routing by teams.")
}

func locationWords(loc routing.Location) string {
	if loc.Kind == routing.Repository {
		return routing.Normalize(loc.Repository) + " (a repository)"
	}
	return "outside any repository with a hosted origin"
}

// explainRoute says what Route decides for loc under st, and why.
func explainRoute(w io.Writer, st routing.State, names teamNames, loc routing.Location, now time.Time) {
	d := routing.NewRouter(st).Route(loc, now)
	fmt.Fprintf(w, "Location:  %s\n", locationWords(loc))
	var team *routing.Team
	if i := slices.IndexFunc(st.Teams, func(t routing.Team) bool { return t.EnrollmentID == d.EnrollmentID }); i >= 0 {
		team = &st.Teams[i]
	}
	switch {
	case d.Outcome == routing.Send:
		fmt.Fprintf(w, "Goes to:   %s\n", teamLabel(names, *team))
	case d.Outcome == routing.Hold && team != nil && d.Reason == routing.ReasonPending:
		fmt.Fprintf(w, "Goes to:   held, then dropped: %s's entry needs a binding no one has decided\n", teamLabel(names, *team))
	case d.Outcome == routing.Hold && team != nil:
		fmt.Fprintf(w, "Goes to:   held, then dropped: %s would collect it, but is %s\n", teamLabel(names, *team), strings.ReplaceAll(string(d.Blocked), "_", " "))
	default:
		fmt.Fprintln(w, "Goes to:   nothing leaves this machine")
	}
	if d.Entry != "" {
		fmt.Fprintf(w, "Entry:     %s\n", d.Entry)
	}
	if line := authorityLine(d.Authority, names); line != "" {
		fmt.Fprintf(w, "Binding:   %s\n", line)
	}
	for _, why := range routeReasons(st, names, loc, d) {
		fmt.Fprintf(w, "  - %s\n", why)
	}
	fmt.Fprintf(w, "Unknown:   %s\n", unknownRule(st, now))
}

// authorityLine says who decides about the repository, and by which binding.
func authorityLine(a routing.Authority, names teamNames) string {
	org := cmp.Or(names.Orgs[a.Organization], a.Organization)
	switch a.Kind {
	case routing.Bound:
		return a.Key + " is bound to " + org + ": only it decides, and personal never collects it"
	case routing.RefusedAll:
		return a.Key + " is refused to every organization: personal may collect it"
	case routing.Pending:
		return a.Key + " is undecided: `terma teams --bind " + a.Key + "=<organization>` or `terma teams --refuse " + a.Key + "`"
	case routing.Free:
		return "nothing binds it to an organization: personal may collect it"
	}
	return ""
}

// routeReasons says why each ticked organization whose entry matches loc did not collect
// it, why a wildcard needs its owner bound, and when personal collects under an owner
// some of whose repositories are bound to an organization.
func routeReasons(st routing.State, names teamNames, loc routing.Location, d routing.Decision) []string {
	repo := routing.Normalize(loc.Repository)
	if loc.Kind != routing.Repository || routing.Owner(repo) == "" {
		return nil
	}
	owner := routing.Owner(repo)
	var out []string
	for _, t := range st.Teams {
		if t.Personal || t.EnrollmentID == d.EnrollmentID || t.Policy.Global() {
			continue
		}
		for _, raw := range t.Policy.Repositories {
			e, ok := routing.ParseEntry(raw, t.Policy.Wildcards)
			if !ok || !e.Matches(repo) {
				continue
			}
			org := cmp.Or(names.Orgs[t.OrganizationID], t.OrganizationID)
			label := teamLabel(names, t) + " lists " + raw
			switch {
			case d.Authority.Kind == routing.Bound && d.Authority.Organization != t.OrganizationID:
				out = append(out, label+", but "+d.Authority.Key+" is bound to "+cmp.Or(names.Orgs[d.Authority.Organization], d.Authority.Organization))
			case d.Authority.Kind == routing.RefusedAll:
				out = append(out, label+", but "+d.Authority.Key+" is refused to every organization")
			case e.Wildcard() && d.Authority.Kind == routing.Bound && hasBinding(st, owner):
				out = append(out, label+": a wildcard collects only once its owner "+owner+" is bound to "+org+", and "+
					owner+" is "+bindingWords(st.Bindings[owner], names)+", so binding "+repo+" alone to "+org+" collects nothing here")
			case e.Wildcard():
				out = append(out, label+": a wildcard collects only once its owner "+owner+" is bound to "+org+": `terma teams --bind "+owner+"="+quoteArg(org)+"`")
			default:
				out = append(out, label+", but a more specific entry or an earlier team decided")
			}
		}
	}
	if d.Reason == routing.ReasonPersonal {
		var bound []string
		for _, k := range slices.Sorted(maps.Keys(st.Bindings)) {
			if b := st.Bindings[k]; !b.Refused && k != owner && routing.Owner(k) == owner {
				bound = append(bound, k+" ("+cmp.Or(names.Orgs[b.Organization], b.Organization)+")")
			}
		}
		if len(bound) > 0 {
			out = append(out, "personal collects this, though other repositories under "+owner+" are bound to an organization: "+strings.Join(bound, ", "))
		}
	}
	return out
}

func hasBinding(st routing.State, key string) bool {
	_, ok := st.Bindings[key]
	return ok
}

// unknownRule says whether a record whose location is unknown, such as one sent before
// its session's first hook, may go to personal here, and if not, why not.
func unknownRule(st routing.State, now time.Time) string {
	if routing.UnknownToPersonal(st, now) {
		return "a record whose location is unknown goes to personal, the only ticked team, which collects everything"
	}
	const held = "a record whose location is unknown is held, then dropped: "
	n := len(st.Teams)
	switch {
	case n == 0 || !st.Teams[n-1].Personal:
		return held + "personal is not ticked"
	case n > 1:
		return held + "personal is not the only ticked team"
	case !st.Teams[0].Policy.Global():
		return held + "personal collects only the repositories it lists"
	case st.Teams[0].StateAt(now) != routing.Active:
		return held + "personal cannot send now"
	}
	return held + "something is bound to an organization, and the record could be that organization's: unbinding it lets personal take them"
}
