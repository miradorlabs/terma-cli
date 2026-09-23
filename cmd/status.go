package cmd

import (
	"fmt"
	"slices"
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
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

// routedPerRepo reports whether per-repo routing is *configured* for a harness and
// project — a routing record that names it, plus a stored key. It does not check that
// the delivery mechanism is actually live; see perRepoLive. Configured-but-not-live is a
// real state (the shims are installed but their directory is not yet on PATH), and
// treating it as "connected" is exactly the lie that made doctor report a repo as fine
// while its sessions still used the global config.
func routedPerRepo(name, projectID string) bool {
	if projectID == "" {
		return false
	}
	// OpenCode routes itself: a per-repo plugin plus this project's key means a session
	// here reports to this project, even though the plugin file names no fixed project.
	if name == "opencode" {
		if keystore.GetFor("opencode", projectID) == "" {
			return false
		}
		st, err := (harness.OpenCode{}).Status()
		return err == nil && st.Exists
	}
	if !shim.Routable(name) {
		return false
	}
	rec, ok, err := shim.LoadRecord(projectID)
	if err != nil || !ok {
		return false
	}
	if slices.Contains(rec.Harnesses, name) {
		return keystore.GetFor(name, projectID) != ""
	}
	return false
}

// repoAsks reports whether the repository at root carries a committed policy that switches
// a harness's signals on. It is the other half of a machine-wide connect made with
// `--exports repos`: that connect holds the endpoint and the key and exports nothing, so
// whether a session here sends anything is this file's decision.
func repoAsks(h harness.Harness, root string) bool {
	scoped, ok := h.(harness.Scoped)
	if !ok || root == "" {
		return false
	}
	st, err := scoped.Local(root).Status()
	return err == nil && len(st.Signals) > 0
}

// perRepoLive reports whether a configured harness's per-repo routing actually fires for
// a session started now. OpenCode's plugin routes on its own; the shim-delivered agents
// (Claude Code, Codex) route only when their name resolves to terma's shim — the shim
// directory ahead of the real binary on PATH — or the shell wrapper is loaded.
func perRepoLive(name string) bool {
	if name == "opencode" {
		return true
	}
	return shim.Active(name)
}

// statusHooks is status's wording for the commit-hook verdict, and whether commit
// stamping counts toward coverage. A plan that could not be computed is not "nothing
// left to write": status used to read it that way and credit commit stamping while
// doctor failed the same repository — and an unreadable hooks file is exactly what
// makes a plan fail. Reinstalling cannot fix what it cannot read, so the reason is
// given rather than the usual advice.
func statusHooks(w hookWiring) (string, bool) {
	switch {
	case w.err != nil:
		return "could not be checked — " + w.err.Error(), false
	case w.changes == 0 && !w.unpointed:
		return "wired", true
	default:
		return "NOT wired (run `terma install`)", false
	}
}

// statusAgent describes one agent in a line, and reports whether its spend actually
// reaches this project. Exporting to the right host but the wrong project is the case
// worth spelling out: everything looks wired, and none of the spend arrives. `terma
// doctor` fails on it, so status must not call it connected. bound says the CLI
// stands in an installed repository.
func statusAgent(v harnessVerdict, bound bool) (string, bool) {
	if v.emissionProblem != "" {
		return "→ " + v.emissionProblem + " — " + v.emissionFix, false
	}
	const pending = "→ per-repo routing configured, but terma's shim is not ahead of it on your PATH"
	switch v.route {
	case routeLive:
		// Only call it connected when the routing actually fires. The machine-wide
		// config already delivering here is the plainer thing to say when it does.
		if v.sendsGlobally {
			return "→ connected", true
		}
		return "→ connected (per-repo routing)", true
	case routeGlobal:
		return "→ connected", true
	case routePending:
		return pending, false
	case routeOtherProject:
		if v.routed {
			return pending, false
		}
		return "→ reporting to project " + v.otherProject + ", not this one — run `terma install`", false
	case routeRepoDecides:
		// A machine-wide connect that exports no signal of its own is "connected" in
		// general and says nothing about *here*. In a bound repository the question
		// has an answer — this repository routes the agent, asks for it, or neither —
		// and doctor gives it; status must give the same one, or it reports readiness
		// for a repository whose sessions send nothing.
		if bound && !v.repoAsks {
			return "→ no telemetry: this repository neither routes it nor asks for it — sessions here send nothing (run `terma install`)", false
		}
		// Pointed somewhere, holding a key, exporting no signal. Nothing but a
		// repository's own policy can make this send, which is `--exports repos`
		// however it was arrived at. Saying "connected" alone would read as working;
		// saying "not connected" would read as broken. It is neither.
		return "→ connected; repositories decide what is sent", true
	}
	if v.err != nil {
		return "(error)", false
	}
	return "→ not connected", false
}

// statusLineSummary is one line on whether Claude Code's status line feeds terma the
// plan's usage windows, and why not when it does not.
func statusLineSummary(v statusLineVerdict) string {
	switch v.capture {
	case statusLineUnknown:
		return "unknown (" + v.err.Error() + ")"
	case statusLineOverridden:
		return "overridden here by " + strings.Join(v.overrides, ", ") + " — plan usage is not captured in this repository"
	case statusLineBehind:
		return "capturing plan usage; your own (" + output.SanitizeTerminal(v.renderer) + ") runs behind it"
	case statusLineDefault:
		return "capturing plan usage (terma's default line)"
	case statusLineReplaced:
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
			out := cmd.OutOrStdout()
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
					fmt.Fprintf(out, "Account:     %s in %s\n", firstNonEmpty(cred.UserEmail, "signed in"), nameOrID(cfg.OrganizationName, cred.OrganizationID))
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
			root, gitDir, repoErr := repoHere(ctx, "")
			hooksOK := false
			var agentHooks doctor.Check
			repoBound := false
			projectID := cfg.ProjectID
			if repoErr != nil {
				fmt.Fprintln(out, "Repository:  not inside a git repository")
			} else if bound, err := termaproject.Load(root); err != nil {
				fmt.Fprintf(out, "Repository:  %s — not installed (run `terma install`)\n", root)
			} else {
				projectID, repoBound = bound.Project.ID, true
				fmt.Fprintf(out, "Repository:  %s → %s\n", root, nameOrID(bound.Project.Name, bound.Project.ID))
				wiring := judgeHookWiring(ctx, root, bound)
				var state string
				state, hooksOK = statusHooks(wiring)
				fmt.Fprintf(out, "Hooks:       %s via %s\n", state, wiring.manager)
				// The agents' own hooks, judged the way doctor judges them: an agent that
				// cannot run its hooks yet costs its share of commit stamping in both.
				if agentHooks = agentHooksCheck(root, bound, cfg.Harnesses); agentHooks.Status == doctor.Warn {
					fmt.Fprintf(out, "Agent hooks: %d of %d agents can run theirs — %s\n", agentHooks.Ready, agentHooks.Of, agentHooks.Fix)
				}
				store := session.Open(gitDir)
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

			// Harnesses.
			var connected []string
			verdicts := judgeHarnesses(ctx, cfg.OTLPURL, projectID, root)
			for _, v := range verdicts {
				suffix, ok := statusAgent(v, repoBound)
				if ok {
					connected = append(connected, v.displayName)
				}
				fmt.Fprintf(out, "Agent:       %s %s\n", v.displayName, suffix)
				if v.name == "claude" && ok {
					fmt.Fprintf(out, "Status line: %s\n", statusLineSummary(judgeStatusLine(root)))
				}
			}
			if len(connected) == 0 {
				fmt.Fprintln(out, "Agent:       none connected — run `terma install`")
			}
			routing := shellRoutingCheck(verdicts, repoBound, cfg.Harnesses)
			if routing.Status != doctor.Skip {
				fmt.Fprintf(out, "Routing:     %s\n", routing.Detail)
				if routing.Fix != "" {
					fmt.Fprintf(out, "             → %s\n", routing.Fix)
				}
			}
			// A repository's own policy narrows what its sessions ship. Said next to
			// the agent it applies to, since the global line cannot show it.
			if repoErr == nil {
				for _, h := range harness.All() {
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

			export := doctorHarnessCheck(verdicts, cfg.OTLPURL, projectID, repoBound)
			checks := []doctor.Check{doctorBinaryCheck(), export, agentHooks, routing,
				{Key: doctor.KeyBackend, Status: doctor.Skip}}
			if !authOK {
				checks = append(checks, doctor.Check{Status: doctor.Fail, Fix: "terma setup"})
			}
			if !repoBound || !hooksOK {
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
