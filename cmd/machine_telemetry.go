package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/output"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/shim"
	"github.com/miradorlabs/terma-cli/internal/spinner"
)

// Every agent's telemetry is configured once, machine-wide: `terma setup` writes each
// agent's global configuration, pointed at terma's loopback relay (which delivers each
// record to the project of the repository its session ran in, docs/RELAY.md) or, with
// --no-relay or where no service manager can run one, straight at Terma with the machine
// project's key. Nothing about telemetry is written per repository any more: Codex
// Desktop, the ChatGPT app and Claude Desktop never passed through a PATH shim, and they
// all read the same global files.

// The relay's API, replaceable in tests: the real ones install a launchd agent or a
// systemd unit and talk to a process on loopback.
var (
	relaySupported      = relay.Supported
	relayEnsure         = relay.Ensure
	relayLoad           = relay.Load
	relayInstallService = relay.InstallService
	relayRestartService = relay.RestartService
	relayService        = relay.Service
	relayProbe          = relay.Probe
	// Files under terma's config directory, which every test sandboxes: not stubbed.
	relaySaveContentPolicy = relay.SaveContentPolicy
	relayLoadContentPolicy = relay.LoadContentPolicy
)

// machineAgents are the harnesses among a developer's agents whose global configuration
// setup writes: Claude Code and Codex (Codex Desktop reads Codex's). OpenCode routes
// itself per repository through its plugin, so it is not one of them.
func machineAgents(agents []string) []harness.Harness {
	var out []harness.Harness
	for _, name := range telemetryAgentNames(agents) {
		if name == "opencode" {
			continue
		}
		if h, err := harness.Lookup(name); err == nil {
			out = append(out, h)
		}
	}
	return out
}

// machineOptions are what a machine configuration run is allowed to do beyond writing
// the recorded choices.
type machineOptions struct {
	// force replaces another collector's settings in an agent's global file. Terma's own
	// earlier settings are always replaced.
	force        bool
	noStatusLine bool
}

// agentOutcome is what configuring one agent came to.
type agentOutcome struct {
	name, display string
	// skipped says why nothing was written, and fix what the developer does about it.
	skipped, fix string
	// statusLine / notifier are the side effects worth a line.
	statusLine, notifier string
}

// machineResult is what configureMachineTelemetry did.
type machineResult struct {
	telemetry config.Telemetry
	relay     relay.Config
	// relayErr is why the relay could not be used, when it was wanted.
	relayErr error
	agents   []agentOutcome
}

// configureMachineTelemetry writes every one of agents' global configurations from t and
// records t on the profile. The relay is used where it can run and the developer has not
// declined it; a relay that cannot be installed leaves the agents exporting straight to
// Terma rather than to nothing. An agent whose file holds another collector's settings is
// skipped unless opts.force, and said so; it never fails the whole run.
func configureMachineTelemetry(ctx context.Context, errOut io.Writer, cfg *config.Config, agents []string, t config.Telemetry, opts machineOptions) (machineResult, error) {
	res := machineResult{}
	if t.Project.ID == "" {
		return res, errors.New("no project to report to — run `terma setup`")
	}
	signals, err := harness.ParseSignals(t.Signals)
	if err != nil {
		return res, err
	}

	t.Mode = config.TelemetryDirect
	if !t.NoRelay && relaySupported() {
		rc, err := relayEnsure()
		if err == nil {
			err = relayInstallService(ctx, relayBinary())
		}
		if err == nil {
			t.Mode, res.relay = config.TelemetryRelay, rc
		} else {
			res.relayErr = err
		}
	}

	// The machine project's key: the direct export's credential, and in relay mode the
	// one the relay delivers the machine project's records with.
	machine := *cfg
	machine.ProjectID, machine.ProjectName = t.Project.ID, t.Project.Name
	for _, h := range machineAgents(agents) {
		out := agentOutcome{name: h.Name(), display: h.DisplayName()}
		if err := configureMachineAgent(ctx, errOut, &machine, h, signals, t, res.relay, opts, &out); err != nil {
			return res, fmt.Errorf("%s: %w", h.DisplayName(), err)
		}
		res.agents = append(res.agents, out)
	}
	res.telemetry = t
	for _, h := range machineAgents(agents) {
		if h.Name() == "claude" {
			// Under the relay, a user-level SessionStart hook records where every Claude
			// session runs, so one resumed outside its repository is placed where it runs;
			// with no relay there is nothing to read it, and it comes out.
			if err := claudePlacementHook(t.Mode == config.TelemetryRelay); err != nil {
				// Not fatal: without it a resumed-elsewhere session keeps its first placement.
				fmt.Fprintf(errOut, "Warning: Claude Code's placement hook was not written: %v\n", err)
			}
		}
	}
	if t.Mode == config.TelemetryRelay {
		// The machine's content choice is the relay's default: for the machine project,
		// and for every project that has none of its own (`terma install --prompts`).
		if err := relaySaveContentPolicy(relay.MachineRoute, relay.ContentPolicy{
			Prompts: !t.ExcludePrompts, ToolContent: !t.ExcludeToolContent,
		}); err != nil {
			return res, err
		}
	}
	if err := config.UpdateProfile(cfg.ProfileName, func(p *config.Profile) {
		saved := t
		p.Telemetry = &saved
	}); err != nil {
		return res, err
	}
	cfg.Telemetry = t
	return res, nil
}

func configureMachineAgent(
	ctx context.Context, errOut io.Writer, machine *config.Config, h harness.Harness,
	signals []harness.Signal, t config.Telemetry, rc relay.Config, opts machineOptions, out *agentOutcome,
) error {
	key, _, _, _, err := resolveKey(ctx, machine, h, connectFlags{})
	if err != nil {
		return err
	}
	if err := keystore.SetFor(h.Name(), machine.ProjectID, key, keystore.HostsOf(machine)); err != nil {
		return err
	}
	e := harness.Exporter{
		Signals:            signals,
		ProjectID:          machine.ProjectID,
		ResourceAttributes: resourceAttributes(ctx, h, machine, t.Identity),
		IncludePrompts:     !t.ExcludePrompts,
		IncludeToolContent: !t.ExcludeToolContent,
	}
	if t.Mode == config.TelemetryRelay {
		// The relay decides each record's project, so nothing in the agent's file may
		// name one, and the credential it holds is the relay's token, not a Terma key.
		// The agent exports content to the relay, which cannot know a record's project
		// before it places the session; the relay withholds it per project
		// (relay.ContentPolicy), so nothing a project withholds leaves the machine.
		e.Endpoint, e.APIKey, e.JSON = rc.Endpoint(), rc.Token, true
		e.IncludePrompts, e.IncludeToolContent = true, true
		delete(e.ResourceAttributes, harness.AttrProjectID)
		if h.SupportsHeadersHelper() {
			if e.HelperPath, err = harness.RelayHelperFilePath(h); err != nil {
				return err
			}
		}
	} else {
		e.Endpoint, e.APIKey = machine.OTLPURL, key
		if h.SupportsHeadersHelper() {
			if e.HelperPath, err = harness.HelperFilePath(h, machine.ProjectID); err != nil {
				return err
			}
		}
	}

	conflicts, err := h.ConflictsWith(e)
	if err != nil {
		return err
	}
	blocking, _ := partitionConflicts(conflicts)
	// terma's own relay is terma's own even in direct mode (rc is then empty): a machine
	// moving off the relay replaces the relay's settings without --force.
	known := rc
	if known.Port == 0 {
		if loaded, err := relayLoad(); err == nil {
			known = loaded
		}
	}
	foreign := slices.DeleteFunc(blocking, func(c harness.Conflict) bool {
		return c.Clearable && termaEndpoint(c.Value, machine.OTLPURL, known)
	})
	if stuck := unclearable(foreign); len(stuck) > 0 {
		out.skipped = "has settings terma does not change: " + output.SanitizeTerminal(strings.Join(stuck, ", "))
		out.fix = "remove or adjust them, then run `terma setup` again"
		return nil
	}
	if len(foreign) > 0 && !opts.force {
		var keys []string
		for _, c := range foreign {
			keys = append(keys, c.Key)
		}
		out.skipped = "already exports elsewhere (" + output.SanitizeTerminal(strings.Join(keys, ", ")) + ")"
		out.fix = "`terma setup --force` replaces them"
		return nil
	}
	if _, err := backupHarnessConfig(h, machine.OTLPURL); err != nil {
		fmt.Fprintf(errOut, "Warning: could not back up %s's configuration (%v).\n", h.DisplayName(), err)
	}
	// Terma's own earlier settings — a direct export being moved to the relay, a stale
	// per-signal value — are cleared along with, under --force, another collector's.
	if err := h.Connect(e, true); err != nil {
		return err
	}
	switch h.Name() {
	case "claude":
		if !opts.noStatusLine {
			out.statusLine, _ = installStatusLine(errOut)
		}
	case "codex":
		switch changed, err := (harness.Codex{}).InstallCodexNotify(); {
		case err != nil:
			fmt.Fprintf(errOut, "Warning: could not install Codex's funding notifier (%v).\n", err)
		case changed:
			out.notifier = "terma captures plan and quota at the end of each turn; any previous notifier keeps running behind it"
		}
	}
	return nil
}

// termaEndpoint reports whether an endpoint a conflict names is one terma itself
// writes — this machine's ingest host, a built-in environment's, or the relay — so an
// earlier terma configuration is replaced without --force. A per-signal URL counts by
// its base.
func termaEndpoint(value, otlpURL string, rc relay.Config) bool {
	v := strings.TrimRight(strings.TrimSpace(value), "/")
	for _, s := range harness.AllSignals {
		v = strings.TrimSuffix(v, "/v1/"+string(s))
	}
	if v == "" {
		return false
	}
	if v == strings.TrimRight(otlpURL, "/") || (rc.Port != 0 && v == rc.Endpoint()) {
		return true
	}
	if v == "http://127.0.0.1:"+fmt.Sprint(relay.DefaultPort) {
		return true
	}
	_, builtIn := config.EndpointsByOTLP(v)
	return builtIn
}

// relayBinary is the terma the relay service runs: this one, by the path it was
// started with, so a package manager's stable link survives an upgrade.
func relayBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return "terma"
	}
	return exe
}

// machineTelemetryCurrent reports whether every one of agents' global configurations
// already exports where the recorded mode says: the relay, or Terma. install configures
// the machine only when it does not.
func machineTelemetryCurrent(cfg *config.Config, agents []string) bool {
	t := cfg.Telemetry
	if t.Mode == "" || t.Project.ID == "" {
		return false
	}
	want := cfg.OTLPURL
	if t.Mode == config.TelemetryRelay {
		rc, err := relayLoad()
		if err != nil {
			return false
		}
		want = rc.Endpoint()
	}
	for _, h := range machineAgents(agents) {
		st, err := h.Status()
		if err != nil || !st.Connected || strings.TrimRight(st.Endpoint, "/") != want {
			return false
		}
		// Under the relay an agent exports content for the relay to withhold per project;
		// one configured before that (content off at the agent) is not current.
		if t.Mode == config.TelemetryRelay && !st.IncludePrompts {
			return false
		}
	}
	if t.Mode == config.TelemetryRelay {
		if _, ok := relayLoadContentPolicy(relay.MachineRoute); !ok {
			return false
		}
		// A relay set up before the placement hook existed lacks it.
		for _, h := range machineAgents(agents) {
			if h.Name() != "claude" {
				continue
			}
			if path, err := h.ConfigPath(); err == nil {
				if plan, err := hookmgr.PlanClaudeUserHooks(filepath.Dir(path), true); err == nil && !plan.Empty() {
					return false
				}
			}
		}
	}
	return true
}

// chooseMachineProject picks the machine project: --project when given, else the one
// recorded, else the organization's only project, else — with a person there — a picker
// with the recorded one offered first. fallback is what to use when nothing is recorded
// and nobody can be asked (an install's own binding).
func chooseMachineProject(cmd *cobra.Command, cfg *config.Config, ref string, ask bool, fallback config.ProjectRef) (config.ProjectRef, error) {
	ref = strings.TrimSpace(ref)
	current := cfg.Telemetry.Project
	if ref == "" && current.ID != "" && !ask {
		return current, nil
	}
	sp := spinner.New(cmd.ErrOrStderr())
	sp.Start("Loading projects…")
	defer sp.Stop()
	client, err := newClient(cfg)
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) && ref != "" && termaproject.ValidID(ref) {
			return config.ProjectRef{ID: ref}, nil
		}
		return config.ProjectRef{}, err
	}
	projects, err := availableProjects(cmd.Context(), client)
	sp.Stop()
	if err != nil {
		return config.ProjectRef{}, err
	}
	ref2 := func(p *project) config.ProjectRef {
		return config.ProjectRef{ID: p.ID, Name: p.Name, OrganizationID: p.OrganizationID}
	}
	if ref != "" {
		p, err := matchProject(projects, ref)
		if err != nil {
			return config.ProjectRef{}, err
		}
		return ref2(p), nil
	}
	known := slices.ContainsFunc(projects, func(p project) bool { return p.ID == current.ID })
	switch {
	case !ask && known:
		return current, nil
	case !ask && fallback.ID != "":
		return fallback, nil
	case !ask && len(projects) > 1:
		return config.ProjectRef{}, errors.New("this organization has several projects — pass `terma setup --project <name or id>` to choose this machine's")
	}
	id := current.ID
	if !known {
		id = fallback.ID
	}
	p, err := soleOrPick(cmd, projects, id)
	if err != nil {
		return config.ProjectRef{}, err
	}
	return ref2(p), nil
}

// cleanupLegacyRouting removes what per-repository routing left on this machine — the
// PATH shims and the block in the shell's startup file that put them first, the routing
// records, the per-project Claude settings — now that every agent is configured globally.
// It does nothing unless that state is there, so it never edits a startup file on a
// machine that never had it. It reports whether it removed anything.
func cleanupLegacyRouting() (bool, error) {
	dir, err := config.Dir()
	if err != nil {
		return false, err
	}
	present := false
	for _, sub := range []string{"shim", "routing", "claude"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); err == nil {
			present = true
		} else if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	if !present {
		return false, nil
	}
	return true, shim.RemoveAll()
}

// stripRepoPolicy takes the telemetry policy an earlier `terma install` committed out of
// the repository's .claude/settings.json: project settings outrank the user file, so a
// leftover exporter=none would silently switch the machine's export off in this
// repository. Only a policy terma wrote is touched (harness.Claude.TermaPolicy), and in
// it only values terma writes (a developer's own OTEL_LOGS_EXPORTER=console stays). It
// returns the file when it changed.
func stripRepoPolicy(root string) (string, error) {
	claude := harness.Claude{}.Local(root).(harness.Claude)
	ours, err := claude.TermaPolicy()
	if err != nil || !ours {
		return "", err
	}
	path, err := claude.ConfigPath()
	if err != nil {
		return "", err
	}
	res, err := claude.Disconnect()
	if err != nil || res.Removed+res.Restored == 0 {
		return "", err
	}
	return path, nil
}

// printMachineResult says what configureMachineTelemetry did, one line per agent.
func printMachineResult(ui *installUI, res machineResult) {
	switch res.telemetry.Mode {
	case config.TelemetryRelay:
		ui.ok("Relay", "running on "+res.relay.Endpoint()+" — each session reports to its repository's project")
	default:
		if res.relayErr != nil {
			ui.warn("Relay", "could not start ("+res.relayErr.Error()+") — agents export straight to Terma")
			ui.then("Run `terma setup` again to retry the relay.")
		}
	}
	for _, a := range res.agents {
		if a.skipped != "" {
			ui.warn(a.display, "not configured — "+a.skipped)
			ui.then(a.display + ": " + a.fix + ".")
			continue
		}
		where := "exports through the relay"
		if res.telemetry.Mode != config.TelemetryRelay {
			where = "exports to " + nameOrID(res.telemetry.Project.Name, res.telemetry.Project.ID)
		}
		ui.ok(a.display, where)
		if a.statusLine != "" {
			fmt.Fprintln(ui.detail, a.statusLine)
		}
		if a.notifier != "" {
			fmt.Fprintf(ui.detail, "Notifier: %s.\n", a.notifier)
		}
	}
	if len(res.agents) > 0 {
		ui.then("Restart your agents so they read their new telemetry settings.")
	}
}

// claudePlacementHook writes (or removes) terma's user-level SessionStart hook in Claude
// Code's user settings — the one place a hook runs in every directory.
func claudePlacementHook(install bool) error {
	path, err := (harness.Claude{}).ConfigPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	plan, err := hookmgr.PlanClaudeUserHooks(dir, install)
	if err != nil || plan.Empty() {
		return err
	}
	if !install {
		if _, err := os.Stat(path); err != nil {
			return nil
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return hookmgr.Apply(dir, plan)
}
