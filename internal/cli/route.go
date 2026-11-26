package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

type routeExplainView struct {
	Path              string             `json:"path,omitempty"`
	Location          routeLocationView  `json:"location"`
	Outcome           routing.Outcome    `json:"outcome"`
	Reason            string             `json:"reason"`
	Blocked           routing.TeamState  `json:"blocked,omitempty"`
	LastError         routing.ErrorClass `json:"last_error,omitempty"`
	Entry             string             `json:"entry,omitempty"`
	Team              *routeTeamView     `json:"team,omitempty"`
	Notes             []string           `json:"notes,omitempty"`
	Unknown           string             `json:"unknown,omitempty"`
	UnknownToPersonal bool               `json:"unknown_to_personal,omitempty"`
	ServerKey         bool               `json:"server_key,omitempty"`
	StateError        string             `json:"state_error,omitempty"`
	StateReason       string             `json:"state_reason,omitempty"`
	Fix               string             `json:"fix,omitempty"`
}

type routeLocationView struct {
	Kind       routing.LocationKind `json:"kind"`
	Repository string               `json:"repository,omitempty"`
}

type routeTeamView struct {
	EnrollmentID     string `json:"enrollment_id"`
	TeamID           string `json:"team_id"`
	TeamName         string `json:"team_name,omitempty"`
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name,omitempty"`
	Personal         bool   `json:"personal,omitempty"`
	Label            string `json:"label"`
}

func (app *App) newRouteCommand() *cobra.Command {
	route := &cobra.Command{
		Use:    "route",
		Short:  "Explain how terma routes a folder",
		Hidden: true,
	}
	var asJSON bool
	explain := &cobra.Command{
		Use:   "explain [path]",
		Short: "Explain which team a session started in path would go to",
		Long: `Says which ticked team a session started in path (default: here) would go to: the
policy entry that matched, the teams passed over and why, and whether a record whose location is unknown may go to personal.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "."
			if len(args) == 1 {
				path = args[0]
			}
			return app.runRouteExplain(cmd, path, asJSON)
		},
	}
	explain.Flags().BoolVar(&asJSON, "json", false, "print the routing explanation as JSON")
	route.AddCommand(explain)
	return route
}

func (app *App) runRouteExplain(cmd *cobra.Command, path string, asJSON bool) error {
	if app.flags.output != "" {
		switch format, err := app.resolveFormat(); {
		case err != nil:
			return err
		case format == output.FormatJSON:
			asJSON = true
		case format != output.FormatTable:
			return fmt.Errorf("route explain supports table and json output, not %s", format)
		}
	}
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
	loc, err := routeLocationOf(cmd.Context(), abs)
	if err != nil {
		return err
	}
	st, found, loadErr := routing.Load(app.stateDir, cfg.ProfileName)
	lane := app.profileLane(cfg)
	serverKey := cfg.ServerKeySignIn || lane == sendas.NoPersonLane

	var v routeExplainView
	if stateReason, stateErr := routingStateProblem(st, found, loadErr, lane); stateReason != "" {
		v = routeExplainView{
			Path:        abs,
			Location:    routeLocationView{Kind: loc.Kind, Repository: routing.Normalize(loc.Repository)},
			Outcome:     routing.Drop,
			Reason:      "no_state",
			ServerKey:   serverKey,
			StateError:  stateErr,
			StateReason: stateReason,
			Fix:         routeSetupFix(serverKey),
		}
	} else {
		v = buildRouteExplain(abs, st, app.routeNames(cfg), loc, time.Now())
	}
	if asJSON {
		return output.Render(cmd.OutOrStdout(), output.FormatJSON, output.Table{}, v)
	}
	renderRouteExplain(style.Highlight(cmd.OutOrStdout()), v)
	return nil
}

func routingStateProblem(st routing.State, found bool, loadErr error, lane string) (reason, detail string) {
	switch {
	case loadErr != nil:
		return "unreadable", "this machine's routing state cannot be read: " + loadErr.Error()
	case !found:
		return "missing", "this profile has no routing state"
	case lane == "":
		return "signed_out", "not signed in, so this profile's routing state is inactive"
	case lane == sendas.AccountUnreadable:
		return "account_unreadable", "this profile's sign-in and account cannot be read or paired"
	case st.Account != lane:
		return "wrong_account", "this profile's routing state belongs to another account"
	}
	return "", ""
}

func routeSetupFix(serverKey bool) string {
	if serverKey {
		return "TERMA_API_KEY=<the team's server key> terma setup"
	}
	return "terma setup"
}

// routeLocationOf is where a session started in dir runs, as a hook sees it: the checkout's
// hosted origin (with an owner), else outside git. A broken .git marker fails closed.
func routeLocationOf(ctx context.Context, dir string) (routing.Location, error) {
	_, gitDir, err := termaproject.Locate(ctx, dir)
	if err != nil {
		return routing.Location{}, err
	}
	if gitDir != "" {
		if origin := gitx.RepositoryFS(gitDir); routing.Owner(origin) != "" {
			return routing.Location{Kind: routing.Repository, Repository: origin}, nil
		}
	}
	return routing.Location{Kind: routing.OutsideGit}, nil
}

func locationWords(loc routeLocationView) string {
	if loc.Kind == routing.Repository {
		return loc.Repository + " (a repository)"
	}
	return "outside any repository with a hosted origin"
}

// buildRouteExplain runs Router.Route on loc under st and builds the full explanation view.
func buildRouteExplain(path string, st routing.State, names teamNames, loc routing.Location, now time.Time) routeExplainView {
	d := routing.NewRouter(st).Route(loc, now)
	serverKey := st.Account == sendas.NoPersonLane
	v := routeExplainView{
		Path:              path,
		Location:          routeLocationView{Kind: loc.Kind, Repository: routing.Normalize(loc.Repository)},
		Outcome:           d.Outcome,
		Reason:            string(d.Reason),
		Blocked:           d.Blocked,
		Entry:             d.Entry,
		Notes:             routeReasons(st, names, loc, d),
		Unknown:           unknownRule(st, now),
		UnknownToPersonal: routing.UnknownToPersonal(st, now),
		ServerKey:         serverKey,
	}
	if t, ok := st.Team(d.EnrollmentID); ok {
		v.LastError = t.LastError
		v.Team = &routeTeamView{
			EnrollmentID:     t.EnrollmentID,
			TeamID:           t.TeamID,
			TeamName:         names.Teams[t.TeamID],
			OrganizationID:   t.OrganizationID,
			OrganizationName: names.Orgs[t.OrganizationID],
			Personal:         t.Personal,
			Label:            teamLabel(names, t),
		}
		if d.Outcome == routing.Hold {
			v.Fix = routeHoldFix(d, t, names, serverKey)
		}
	}
	return v
}

func routeHoldFix(d routing.Decision, t routing.Team, names teamNames, serverKey bool) string {
	if serverKey {
		return routeSetupFix(true)
	}
	switch d.Blocked {
	case routing.SignInNeeded:
		return "terma setup"
	case routing.Suspended, routing.Refused:
		return "terma teams --release " + quoteArg(cmp.Or(names.Teams[t.TeamID], t.TeamID)) + " (or wait for its policy to come back)"
	case routing.Expired:
		return "check the network; the relay refreshes it every minute"
	}
	return "terma teams"
}

func renderRouteExplain(w io.Writer, v routeExplainView) {
	fmt.Fprintf(w, "Location:  %s\n", locationWords(v.Location))
	if v.StateError != "" {
		fmt.Fprintf(w, "Goes to:   nothing leaves this machine: %s\n", v.StateError)
		if v.Fix != "" {
			fmt.Fprintf(w, "Fix:       %s\n", v.Fix)
		}
		return
	}
	switch {
	case v.Outcome == routing.Send && v.Team != nil:
		fmt.Fprintf(w, "Goes to:   %s\n", v.Team.Label)
	case v.Outcome == routing.Hold && v.Team != nil && v.Blocked == routing.SignInNeeded:
		fmt.Fprintf(w, "Goes to:   held, then dropped: %s would collect it, but needs you to sign in again\n", v.Team.Label)
	case v.Outcome == routing.Hold && v.Team != nil && (v.Blocked == routing.Suspended || v.Blocked == routing.Refused):
		fmt.Fprintf(w, "Goes to:   held, then dropped: %s would collect it, but is %s (%s)\n", v.Team.Label, strings.ReplaceAll(string(v.Blocked), "_", " "), routeRefusalWords(v.LastError))
	case v.Outcome == routing.Hold && v.Team != nil:
		fmt.Fprintf(w, "Goes to:   held, then dropped: %s would collect it, but is %s\n", v.Team.Label, strings.ReplaceAll(string(v.Blocked), "_", " "))
	default:
		fmt.Fprintln(w, "Goes to:   nothing leaves this machine")
	}
	if v.Entry != "" {
		fmt.Fprintf(w, "Entry:     %s\n", v.Entry)
	}
	if v.ServerKey {
		fmt.Fprintln(w, "Profile:   server key (one team)")
	}
	for _, why := range v.Notes {
		fmt.Fprintf(w, "  - %s\n", why)
	}
	if v.Fix != "" {
		fmt.Fprintf(w, "Fix:       %s\n", v.Fix)
	}
	fmt.Fprintf(w, "Unknown:   %s\n", v.Unknown)
}

func routeRefusalWords(c routing.ErrorClass) string {
	switch c {
	case routing.OrgAccessLost:
		return orgInaccessibleNote
	case routing.Terminal:
		return "its organization says the team is gone"
	}
	return "its refreshes were refused"
}

// routeReasons names each other ticked team whose policy lists the location, and why
// the team Route chose collects it instead: that team is refused, or its policy is older.
func routeReasons(st routing.State, names teamNames, loc routing.Location, d routing.Decision) []string {
	repo := routing.Normalize(loc.Repository)
	if loc.Kind != routing.Repository || routing.Owner(repo) == "" {
		return nil
	}
	var out []string
	for _, t := range st.Teams {
		if t.Personal || t.EnrollmentID == d.EnrollmentID || t.Policy.Global() {
			continue
		}
		for _, raw := range t.Policy.Repositories {
			if e, ok := routing.ParseEntry(raw, t.Policy.Wildcards); ok && e.Matches(repo) {
				why := ", but a more recently updated policy collects it"
				if t.StateAt(time.Now()) == routing.Refused {
					why = ", but it is refused (" + routeRefusalWords(t.LastError) + "), so it is passed over"
				}
				out = append(out, teamLabel(names, t)+" lists "+raw+why)
				break
			}
		}
	}
	return out
}

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
	return held + "personal cannot take it"
}
