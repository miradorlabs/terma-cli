package cli

import (
	"cmp"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/harness"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

// statusHooks words the commit-hook verdict and whether stamping counts toward coverage;
// a plan that could not be computed (an unreadable hooks file) is reported, not credited.
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

// statusAgent describes one agent in a line and whether its spend reaches this project,
// using doctor's own judgement (HarnessVerdict.Reaches) so the two cannot disagree.
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
		// In a bound repository status must give doctor's answer for this repository.
		if bound && !v.RepoAsks {
			return "→ no telemetry: this repository neither routes it nor asks for it — sessions here send nothing (run `terma install`)"
		}
		// Neither "connected" (working) nor "not connected" (broken): repositories decide.
		return "→ connected; repositories decide what is sent"
	}
	if v.Err != nil {
		return "(error)"
	}
	return "→ not connected"
}

// statusLineSummary says whether the agent's status line feeds terma the plan's usage windows.
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

func (app *App) newStatusCommand() *cobra.Command {
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
			cfg, err := app.loadConfig()
			if err != nil {
				return err
			}

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
			// Name the backend whenever it is not production, by environment or by host overrides.
			switch {
			case cfg.Environment != config.EnvProd:
				fmt.Fprintf(out, "Environment: %s (%s)\n", cfg.Environment, cfg.AuthURL)
			case cfg.AuthURL != config.DefaultAuthURL:
				fmt.Fprintf(out, "Endpoints:   custom, from profile %s (%s)\n", cfg.ProfileName, cfg.AuthURL)
			}

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
				// An agent that cannot run its hooks yet costs its share of commit stamping.
				if agentHooks = doctor.AgentHooksCheck(app.agents, root, doctor.SelectedForRepo(app.agents, projectID, cfg.Harnesses)); agentHooks.Status == doctor.Warn {
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

			// Through the relay one line gives doctor's own verdict (doctor.RelayCheck).
			var export doctor.Check
			if claim.Enabled() {
				export = doctor.RelayCheck(app.agents, projectID, cfg.Harnesses)
				export.Key = doctor.KeyHarness
				fmt.Fprintf(out, "Agents:      %s\n", export.Detail)
				if export.Fix != "" {
					fmt.Fprintf(out, "             → %s\n", export.Fix)
				}
			} else {
				var connected []string
				verdicts := doctor.JudgeSelectedHarnesses(ctx, app.agents, cfg.OTLPURL, projectID, root, cfg.Harnesses)
				for _, v := range verdicts {
					suffix, ok := statusAgent(v, repoBound)
					if ok {
						connected = append(connected, v.DisplayName)
					}
					fmt.Fprintf(out, "Agent:       %s %s\n", v.DisplayName, suffix)
					if a, lines := doctor.StatusLineAgent(app.agents); lines && v.Name == a.Name() && ok {
						fmt.Fprintf(out, "Status line: %s\n", statusLineSummary(doctor.JudgeStatusLine(app.agents, root)))
					}
				}
				if len(connected) == 0 {
					fmt.Fprintln(out, "Agent:       none connected — run `terma install`")
				}
				export = doctor.HarnessCheck(app.agents, verdicts, cfg.OTLPURL, projectID, repoBound)
			}
			// A repository's own policy narrows what its sessions ship; the global line cannot show it.
			if repoErr == nil {
				for _, h := range app.agents.Harnesses() {
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

			checks := []doctor.Check{app.binaryCheck(), export, agentHooks,
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
