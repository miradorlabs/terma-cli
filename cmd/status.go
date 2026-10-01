package cmd

import (
	"cmp"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/output"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/style"
)

// statusHooks is status's wording for the commit-hook verdict, and whether commit
// stamping counts toward coverage. A plan that could not be computed is not "nothing
// left to write": status used to read it that way and credit commit stamping while
// doctor failed the same repository — and an unreadable hooks file is exactly what
// makes a plan fail. Reinstalling cannot fix what it cannot read, so the reason is
// given rather than the usual advice.
func statusHooks(w doctor.HookWiring) (string, bool) {
	switch {
	case w.Err != nil:
		return "could not be checked — " + w.Err.Error(), false
	case w.Changes == 0 && !w.Unpointed:
		return "wired", true
	case w.Changes > 0 && w.Stale == w.Changes && !w.Unpointed:
		return "out of date (run `terma update --refresh`)", false
	default:
		return "NOT wired (run `terma install`)", false
	}
}

// statusAgent describes one agent in a line, and reports whether its spend actually
// reaches this project. Exporting to the right host but the wrong project is the case
// worth spelling out: everything looks wired, and none of the spend arrives. `terma
// doctor` fails on it, so status must not call it connected. bound says the CLI
// stands in an installed repository. Whether it reaches the project is doctor's
// judgement (HarnessVerdict.Reaches), so the two cannot disagree.
func statusAgent(v doctor.HarnessVerdict, bound bool) (string, bool) {
	return statusAgentLine(v, bound), v.Reaches(bound)
}

func statusAgentLine(v doctor.HarnessVerdict, bound bool) string {
	if v.EmissionProblem != "" {
		return "→ " + v.EmissionProblem + " — " + v.EmissionFix
	}
	switch v.Route {
	case doctor.RouteGlobal:
		return "→ connected"
	case doctor.RouteHooks:
		return "→ connected (repository hooks)"
	case doctor.RouteOtherProject:
		return "→ reporting to project " + v.OtherProject + ", not this one — run `terma install`"
	case doctor.RouteRepoDecides:
		// A machine-wide connect that exports no signal of its own is "connected" in
		// general and says nothing about *here*. In a bound repository the question
		// has an answer — this repository routes the agent, asks for it, or neither —
		// and doctor gives it; status must give the same one, or it reports readiness
		// for a repository whose sessions send nothing.
		if bound && !v.RepoAsks {
			return "→ no telemetry: this repository neither routes it nor asks for it — sessions here send nothing (run `terma install`)"
		}
		// Pointed somewhere, holding a key, exporting no signal. Nothing but a
		// repository's own policy can make this send, which is `--exports repos`
		// however it was arrived at. Saying "connected" alone would read as working;
		// saying "not connected" would read as broken. It is neither.
		return "→ connected; repositories decide what is sent"
	}
	if v.Err != nil {
		return "(error)"
	}
	return "→ not connected"
}

// statusLineSummary is one line on whether Claude Code's status line feeds terma the
// plan's usage windows, and why not when it does not.
func statusLineSummary(v doctor.StatusLineVerdict) string {
	switch v.Capture {
	case doctor.StatusLineUnknown:
		return "unknown (" + v.Err.Error() + ")"
	case doctor.StatusLineOverridden:
		return "overridden here by " + strings.Join(v.Overrides, ", ") + " — plan usage is not captured in this repository"
	case doctor.StatusLineBehind:
		return "capturing plan usage; your own (" + output.SanitizeTerminal(v.Renderer) + ") runs behind it"
	case doctor.StatusLineDefault:
		return "capturing plan usage (terma's default line)"
	case doctor.StatusLineReplaced:
		return "replaced by your own since terma wrapped it — plan usage is NOT captured (run `terma install`)"
	}
	return "not wrapped — plan usage is NOT captured (run `terma install`)"
}

func newStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show connections, queued events, and setup readiness",
		Long: `A quick, local view of this machine and repository: sign-in, project binding,
hook wiring, connected agents, the event spool, and remaining setup steps.
Nothing is written and no scratch commit is made — run
` + "`terma doctor`" + ` for the end-to-end verification.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := style.Highlight(cmd.OutOrStdout())
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			// Account.
			authOK := true
			switch {
			case cfg.APIKey != "":
				fmt.Fprintln(out, "Account:     server key (TERMA_API_KEY)")
			default:
				cred, err := auth.LoadCredential(cfg.ProfileName)
				if err != nil {
					authOK = false
					fmt.Fprintln(out, "Account:     not signed in — run `terma setup`")
				} else {
					fmt.Fprintf(out, "Account:     %s in %s\n", cmp.Or(cred.UserEmail, "signed in"), cmp.Or(cfg.OrganizationName, cred.OrganizationID))
				}
			}
			// Say where this is pointed whenever it is not production, by a named
			// environment or by endpoint overrides on the profile. Someone reading
			// their own status should never have to guess which backend it describes,
			// and the environment name is only the source of the defaults — once a
			// profile overrides the hosts, naming it "prod" would be a lie.
			switch {
			case cfg.Environment != config.EnvProd:
				fmt.Fprintf(out, "Environment: %s (%s)\n", cfg.Environment, cfg.AuthURL)
			case cfg.AuthURL != config.DefaultAuthURL:
				fmt.Fprintf(out, "Endpoints:   custom, from profile %s (%s)\n", cfg.ProfileName, cfg.AuthURL)
			}

			// Repository.
			root, gitDir, repoErr := workspaceHere(ctx)
			hooksOK := false
			var agentHooks doctor.Check
			repoBound := false
			projectID := cfg.ProjectID
			if repoErr != nil {
				fmt.Fprintln(out, "Repository:  not inside a git repository")
			} else if bound, from, err := termaproject.Resolve(root, gitDir); err != nil {
				fmt.Fprintf(out, "Repository:  %s — not installed (run `terma install`)\n", root)
			} else {
				projectID, repoBound = bound.Project.ID, true
				fmt.Fprintf(out, "Repository:  %s → %s%s\n", root, cmp.Or(bound.Project.Name, bound.Project.ID), doctor.ThroughMain(root, from))
				if gitDir == "" {
					fmt.Fprintln(out, "Hooks:       Git hooks skipped (not a Git repository)")
				} else {
					wiring := doctor.JudgeHookWiring(ctx, root, bound)
					var state string
					state, hooksOK = statusHooks(wiring)
					fmt.Fprintf(out, "Hooks:       %s via %s\n", state, wiring.Manager)
				}
				// The agents' own hooks, judged the way doctor judges them: an agent that
				// cannot run its hooks yet costs its share of commit stamping in both.
				if agentHooks = doctor.AgentHooksCheck(registered, root, doctor.SelectedForRepo(registered, projectID, cfg.Harnesses)); agentHooks.Status == doctor.Warn {
					fmt.Fprintf(out, "Agent hooks: %d of %d agents can run theirs — %s\n", agentHooks.Ready, agentHooks.Of, agentHooks.Fix)
				}
				stateDir, err := termaproject.StateDir(root, gitDir)
				if err != nil {
					return err
				}
				store := session.Open(stateDir)
				if active, fresh := store.Active(time.Now(), 4*time.Hour); active != nil && fresh {
					fmt.Fprintf(out, "Session:     %s (%s), active\n", active.ID, active.ToolLabel())
				}
				if manifests, _ := store.Manifests(); len(manifests) > 0 {
					pending, sessions := 0, 0
					for _, m := range manifests {
						if len(m.Files) > 0 {
							pending += len(m.Files)
							sessions++
						}
					}
					if pending > 0 {
						fmt.Fprintf(out, "Uncommitted: %d agent-edited file(s) across %d session(s)\n", pending, sessions)
					}
				}
			}

			// Harnesses. Through the local relay one line says it all, the same verdict
			// doctor gives (relayDoctorCheck), so the two never disagree.
			var export doctor.Check
			if claim.Enabled() {
				export = doctor.RelayCheck(registered, projectID, cfg.Harnesses)
				export.Key = doctor.KeyHarness
				fmt.Fprintf(out, "Agents:      %s\n", export.Detail)
				if export.Fix != "" {
					fmt.Fprintf(out, "             → %s\n", export.Fix)
				}
			} else {
				var connected []string
				verdicts := doctor.JudgeSelectedHarnesses(ctx, registered, cfg.OTLPURL, projectID, root, cfg.Harnesses)
				for _, v := range verdicts {
					suffix, ok := statusAgent(v, repoBound)
					if ok {
						connected = append(connected, v.DisplayName)
					}
					fmt.Fprintf(out, "Agent:       %s %s\n", v.DisplayName, suffix)
					if a, lines := doctor.StatusLineAgent(registered); lines && v.Name == a.Name() && ok {
						fmt.Fprintf(out, "Status line: %s\n", statusLineSummary(doctor.JudgeStatusLine(registered, root)))
					}
				}
				if len(connected) == 0 {
					fmt.Fprintln(out, "Agent:       none connected — run `terma install`")
				}
				export = doctor.HarnessCheck(registered, verdicts, cfg.OTLPURL, projectID, repoBound)
			}
			// A repository's own policy narrows what its sessions ship. Said next to
			// the agent it applies to, since the global line cannot show it.
			if repoErr == nil {
				for _, h := range registered.Harnesses() {
					scoped, ok := h.(harness.Scoped)
					if !ok {
						continue
					}
					st, err := scoped.Local(root).Status()
					if err != nil || st.ManagedKeys == 0 {
						continue
					}
					fmt.Fprintf(out, "Local:       %s ships %s from this repository (%s)\n",
						h.DisplayName(), describeShipment(st), gitx.Relativize(root, st.ConfigPath))
				}
			}

			// Spool.
			backendOK := false
			if s := openSpool(); s != nil {
				backendOK = true
				n, _, _ := s.Pending()
				line := fmt.Sprintf("Spool:       %d event(s) queued", n)
				if next := s.NextAttempt(); !next.IsZero() && time.Now().Before(next) {
					line += fmt.Sprintf(", delivery failing (retry at %s)", next.Local().Format(time.Kitchen))
					backendOK = false
				} else if next, open := s.RetryWindows(time.Now())[projectID]; open && projectID != "" {
					line += fmt.Sprintf(", delivery failing for this project (retry at %s)", next.Local().Format(time.Kitchen))
					backendOK = false
				}
				if projectID != "" {
					if key := keystore.Get(projectID); key != "" {
						line += ", key " + keystore.Mask(key)
					} else {
						line += ", no project key (run `terma install`)"
						backendOK = false
					}
				}
				fmt.Fprintln(out, line)
			}

			checks := []doctor.Check{binaryCheck(), export, agentHooks,
				{Key: doctor.KeyBackend, Status: doctor.Skip}}
			if !authOK {
				checks = append(checks, doctor.Check{Status: doctor.Fail, Fix: "terma setup"})
			}
			if !repoBound || (gitDir != "" && !hooksOK) {
				checks = append(checks, doctor.Check{Status: doctor.Fail, Fix: "terma install"})
			}
			if !backendOK {
				checks = append(checks, doctor.Check{Status: doctor.Warn, Fix: "terma doctor"})
			}
			doctor.RenderSummary(out, doctor.Build(checks))
			fmt.Fprintln(out, "Run `terma doctor` to verify hook execution and backend delivery.")
			return nil
		},
	}
}
