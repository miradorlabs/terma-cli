package doctor

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/harness"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// Row is one labelled line of context above doctor's checks; a row with no label continues
// the one before it.
type Row struct{ Label, Value string }

// LocalReport is `terma status`: this machine and repository from local state alone, in
// doctor's own verdicts, so the two cannot disagree. No scratch commit, no network.
type LocalReport struct {
	Rows   []Row
	Checks []Check
}

// Local reports env's machine and repository; env.Config must be loaded.
func Local(ctx context.Context, env Env) (LocalReport, error) {
	var rep LocalReport
	add := func(label, format string, args ...any) {
		rep.Rows = append(rep.Rows, Row{label, fmt.Sprintf(format, args...)})
	}
	cfg, p := env.Config, env.Probes
	authOK := true
	switch {
	case cfg.APIKey != "":
		add("Account", "server key (TERMA_API_KEY)")
	case p.Credential == nil:
		authOK = false
		add("Account", "not signed in — run `terma setup`")
	default:
		if cred, err := p.Credential(); err != nil {
			authOK = false
			add("Account", "not signed in — run `terma setup`")
		} else {
			add("Account", "%s in %s", cmp.Or(cred.Email, "signed in"), cmp.Or(cfg.OrganizationName, cred.OrganizationID))
		}
	}
	rep.Rows = append(rep.Rows, machineRows(cfg)...)

	root, gitDir, reg := env.Root, env.GitDir, env.Agents
	hooksOK, repoBound, projectID := false, false, cfg.ProjectID
	var agentHooks Check
	if env.RepoErr != nil {
		add("Repository", "not inside a git repository")
	} else if bound, from, err := termaproject.Resolve(root, gitDir); err != nil {
		add("Repository", "%s — not installed (run `terma install`)", root)
	} else {
		projectID, repoBound = bound.Project.ID, true
		add("Repository", "%s → %s%s", root, cmp.Or(bound.Project.Name, bound.Project.ID), ThroughMain(root, from))
		if gitDir == "" {
			add("Hooks", "Git hooks skipped (not a Git repository)")
		} else {
			wiring := JudgeHookWiring(ctx, root, bound)
			var state string
			state, hooksOK = HooksSummary(wiring)
			add("Hooks", "%s via %s", state, wiring.Manager)
		}
		// An agent that cannot run its hooks yet costs its share of commit stamping.
		if agentHooks = AgentHooksCheck(reg, root, SelectedForRepo(reg, projectID, cfg.Harnesses)); agentHooks.Status == Warn {
			add("Agent hooks", "%d of %d agents can run theirs — %s", agentHooks.Ready, agentHooks.Of, agentHooks.Fix)
		}
		work, err := workRows(root, gitDir)
		if err != nil {
			return LocalReport{}, err
		}
		rep.Rows = append(rep.Rows, work...)
	}

	// Through the relay one line gives doctor's own verdict (RelayCheck).
	var export Check
	if claim.Enabled() && p.Relay != nil {
		export = RelayCheck(reg, p.Relay(), p.Keys, projectID, cfg.Environment, cfg.Harnesses)
		export.Key = KeyHarness
		add("Agents", "%s", export.Detail)
		if export.Fix != "" {
			add("", "→ %s", export.Fix)
		}
	} else {
		var connected []string
		verdicts := JudgeHarnesses(ctx, reg, cfg.OTLPURL, projectID, root)
		for _, v := range verdicts {
			suffix, ok := AgentSummary(v, repoBound)
			if ok {
				connected = append(connected, v.DisplayName)
			}
			add("Agent", "%s %s", v.DisplayName, suffix)
			if a, lines := StatusLineAgent(reg); lines && v.Name == a.Name() && ok {
				add("Status line", "%s", StatusLineSummary(JudgeStatusLine(reg, root)))
			}
		}
		if len(connected) == 0 {
			add("Agent", "none connected — run `terma install`")
		}
		export = HarnessCheck(reg, verdicts, cfg.OTLPURL, projectID, repoBound)
	}
	if env.RepoErr == nil {
		rep.Rows = append(rep.Rows, repoPolicyRows(reg, root)...)
	}

	backendOK := false
	if p.Spool != nil {
		if s := p.Spool(); s.Open {
			backendOK = true
			line := fmt.Sprintf("%d event(s) queued", s.Queued)
			if !s.NextAttempt.IsZero() && time.Now().Before(s.NextAttempt) {
				line += fmt.Sprintf(", delivery failing (retry at %s)", s.NextAttempt.Local().Format(time.Kitchen))
				backendOK = false
			} else if next, open := s.Windows[projectID]; open && projectID != "" {
				line += fmt.Sprintf(", delivery failing for this team (retry at %s)", next.Local().Format(time.Kitchen))
				backendOK = false
			}
			if projectID != "" {
				if key := keyOf(p.Keys, projectID); key != "" {
					line += ", key " + key
				} else {
					line += ", no team key (run `terma install`)"
					backendOK = false
				}
			}
			add("Spool", "%s", line)
		}
	}

	rep.Checks = []Check{BinaryCheck(env.Exe, env.BinDirs, HookCallerFor(env.Root, env.GitDir, env.RepoErr)), export, agentHooks, {Key: KeyBackend, Status: Skip}}
	if !authOK {
		rep.Checks = append(rep.Checks, Check{Status: Fail, Fix: "terma setup"})
	}
	if !repoBound || (gitDir != "" && !hooksOK) {
		rep.Checks = append(rep.Checks, Check{Status: Fail, Fix: "terma install"})
	}
	if !backendOK {
		rep.Checks = append(rep.Checks, Check{Status: Warn, Fix: "terma doctor"})
	}
	return rep, nil
}

// Context is what `terma doctor` prints above its checks, which do not cover it: what this
// machine collects, which backend it reports to, and the repository's work in progress.
func Context(env Env) []Row {
	if env.Config == nil {
		return nil
	}
	rows := machineRows(env.Config)
	if env.RepoErr != nil {
		return rows
	}
	if _, _, err := termaproject.Resolve(env.Root, env.GitDir); err == nil {
		if work, err := workRows(env.Root, env.GitDir); err == nil {
			rows = append(rows, work...)
		}
	}
	return append(rows, repoPolicyRows(env.Agents, env.Root)...)
}

// machineRows are what this machine collects and where it reports.
func machineRows(cfg *config.Config) []Row {
	var rows []Row
	switch {
	case cfg.Policy.Validated() && cfg.Policy.Expired(time.Now()):
		rows = append(rows, Row{"Capture", "off: the collection policy has not been refreshed for over a week — run `terma setup`"})
	case cfg.Policy.Validated():
		rows = append(rows, Row{"Collecting", PolicySummary(cfg.Policy)})
	}
	// Name the backend whenever it is not production, by environment or by host overrides.
	switch {
	case cfg.Environment != config.EnvProd:
		rows = append(rows, Row{"Environment", fmt.Sprintf("%s (%s)", cfg.Environment, cfg.AuthURL)})
	case cfg.AuthURL != config.DefaultAuthURL:
		rows = append(rows, Row{"Endpoints", fmt.Sprintf("custom, from profile %s (%s)", cfg.ProfileName, cfg.AuthURL)})
	}
	return rows
}

// workRows are a bound repository's active session and the agent edits not yet committed.
func workRows(root, gitDir string) ([]Row, error) {
	stateDir, err := termaproject.StateDir(root, gitDir)
	if err != nil {
		return nil, err
	}
	var rows []Row
	store := session.Open(stateDir)
	if active, fresh := store.Active(time.Now(), 4*time.Hour); active != nil && fresh {
		rows = append(rows, Row{"Session", fmt.Sprintf("%s (%s), active", active.ID, active.ToolLabel())})
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
			rows = append(rows, Row{"Uncommitted", fmt.Sprintf("%d agent-edited file(s) across %d session(s)", pending, sessions)})
		}
	}
	return rows, nil
}

// repoPolicyRows are the repository's own policies, which narrow what its sessions ship;
// the machine's line cannot show them.
func repoPolicyRows(reg *agents.Registry, root string) []Row {
	var rows []Row
	for _, h := range reg.Harnesses() {
		local, ok := h.Local(root)
		if !ok {
			continue
		}
		st, err := local.Status()
		if err != nil || st.ManagedKeys == 0 {
			continue
		}
		rows = append(rows, Row{"Local", fmt.Sprintf("%s ships %s from this repository (%s)", h.DisplayName(), shipment(st), gitx.Relativize(root, st.ConfigPath))})
	}
	return rows
}

func keyOf(keys Keys, projectID string) string {
	if keys == nil {
		return ""
	}
	return keys("", projectID)
}

// HooksSummary words the commit-hook verdict and whether stamping counts toward
// coverage; a plan that could not be computed (an unreadable hooks file) is reported,
// not credited.
func HooksSummary(w HookWiring) (string, bool) {
	switch {
	case w.Err != nil:
		return "could not be checked — " + w.Err.Error(), false
	case w.Changes == 0 && !w.Unpointed:
		return "wired", true
	case w.Changes > 0 && w.Stale == w.Changes && !w.Unpointed:
		return "out of date (run `terma update`)", false
	default:
		return "NOT wired (run `terma install`)", false
	}
}

// AgentSummary describes one agent in a line and whether its spend reaches this project,
// by the same judgement as the harness check (HarnessVerdict.Reaches).
func AgentSummary(v HarnessVerdict, bound bool) (string, bool) {
	return agentLine(v, bound), v.Reaches(bound)
}

func agentLine(v HarnessVerdict, bound bool) string {
	if v.EmissionProblem != "" {
		return "→ " + v.EmissionProblem + " — " + v.EmissionFix
	}
	switch v.Route {
	case RouteGlobal:
		return "→ connected"
	case RouteHooks:
		return "→ connected (repository hooks)"
	case RouteOtherProject:
		return "→ reporting to team " + v.OtherProject + ", not this one — run `terma install`"
	case RouteRepoDecides:
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

// StatusLineSummary says whether the agent's status line feeds terma the plan's usage
// windows.
func StatusLineSummary(v StatusLineVerdict) string {
	switch v.Capture {
	case StatusLineUnknown:
		return "unknown (" + v.Err.Error() + ")"
	case StatusLineOverridden:
		return "overridden here by " + strings.Join(v.Overrides, ", ") + " — plan usage is not captured in this repository"
	case StatusLineBehind:
		return "capturing plan usage; your own (" + output.SanitizeTerminal(v.Renderer) + ") runs behind it"
	case StatusLineDefault:
		return "capturing plan usage (terma's default line)"
	case StatusLineReplaced:
		return "replaced by your own since terma wrapped it — plan usage is NOT captured (run `terma install`)"
	}
	return "not wrapped — plan usage is NOT captured (run `terma install`)"
}

// shipment is what a repository's own policy lets its sessions ship.
func shipment(st harness.Status) string {
	signals := "nothing"
	if len(st.Signals) > 0 {
		parts := make([]string, 0, len(st.Signals))
		for _, s := range st.Signals {
			parts = append(parts, string(s))
		}
		slices.Sort(parts)
		signals = strings.Join(parts, ",")
	}
	return fmt.Sprintf("%s; prompts %s; tool content %s", signals, onOff(st.IncludePrompts), onOff(st.IncludeToolContent))
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

// PolicySummary is what a collection policy collects, in one line.
func PolicySummary(p config.Policy) string {
	scope := "sessions in connected repositories"
	if p.Global() {
		scope = "every session on this machine"
	}
	switch {
	case p.IncludePrompts && p.IncludeToolContent:
		return scope + ", with prompts and tool content"
	case p.IncludePrompts:
		return scope + ", with prompts, without tool content"
	case p.IncludeToolContent:
		return scope + ", with tool content, without prompts"
	}
	return scope + ", without prompts or tool content"
}
