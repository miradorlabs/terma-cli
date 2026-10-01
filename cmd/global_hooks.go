package cmd

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

// Global mode's machine-wide agent hooks. In global mode the organization collects
// every session on the machine, so the hooks that claim a session cannot live only in
// repositories that opted in: `terma setup` writes terma's entries into each agent's
// user-level hooks file (Claude Code's settings, Codex's $CODEX_HOME/hooks.json,
// Cursor's ~/.cursor/hooks.json), calling `terma hook --user <event>` by absolute path.
//
// For those agents the machine-wide hooks are the ones that act: a repository's own
// committed hooks step aside (hookYields), so nothing is recorded twice. It is not the
// other way round because a repository's hooks do not always run — Codex skips them
// until each developer trusts them — and global mode must not depend on that. The
// agents that got machine-wide hooks are recorded (relay/user-hooks.json), which is how
// a hook knows; leaving global mode removes the entries and the record, and until then
// a machine-wide hook outside global mode does nothing.

const userHooksFile = "user-hooks.json"

type userHooksRecord struct {
	Agents []string `json:"agents"`
}

// userHookAgents are the agents with a user-level hooks file terma writes, among the
// developer's: Codex Desktop counts as Codex (it runs the same hooks).
// userHooksTrustSteps are what the selected agents need of the developer before their
// machine-wide hooks run, unless an organization deployed them as managed configuration.
func (app *App) userHooksTrustSteps(selected []string) []string {
	var steps []string
	for _, name := range app.userHookAgents(selected) {
		if a, ok := app.agents.Find[agents.UserHooksTrust](name); ok && !app.managedHooksDeployed(name) {
			steps = append(steps, a.UserHooksTrustStep())
		}
	}
	return steps
}

func (app *App) userHookAgents(selected []string) []string {
	var out []string
	for _, a := range app.agents.With[agents.UserHooks]() {
		for _, choice := range agents.Selections(a) {
			if slices.Contains(selected, choice) {
				out = append(out, a.Name())
				break
			}
		}
	}
	return out
}

// applyUserHooks writes terma's machine-wide hooks for the developer's agents (install)
// or removes every one terma wrote (not install), and records which agents have them.
// It reports the files it changed.
func (app *App) applyUserHooks(selected []string, install bool) ([]string, error) {
	terma, err := app.hookExecutable()
	if err != nil {
		return nil, err
	}
	covered := app.userHookAgents(selected)
	var changed []string
	for _, a := range app.agents.With[agents.UserHooks]() {
		// Written for the developer's selected in global mode, unless the organization's
		// managed hooks run for one — then setup's would run as well, and go.
		want := install && slices.Contains(covered, a.Name()) && !app.managedHooksDeployed(a.Name())
		path, err := a.UserHooksPath()
		dir, file := filepath.Dir(path), filepath.Base(path)
		if err != nil {
			return changed, err
		}
		if !want {
			if _, err := os.Stat(filepath.Join(dir, file)); errors.Is(err, fs.ErrNotExist) {
				continue
			}
		}
		plan, err := a.PlanUserHooks(dir, hookmgr.UserHookCommand(terma), want)
		if err != nil {
			return changed, err
		}
		if plan.Empty() {
			continue
		}
		if want {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return changed, err
			}
		}
		if err := hookmgr.Apply(dir, plan); err != nil {
			return changed, err
		}
		changed = append(changed, filepath.Join(dir, file))
	}
	rec, err := userHooksRecordPath()
	if err != nil {
		return changed, err
	}
	if !install {
		if err := os.Remove(rec); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return changed, err
		}
		return changed, nil
	}
	// Recorded whichever hooks run for it — setup's or the organization's managed ones:
	// either way a repository's committed hooks step aside for it.
	return changed, config.WriteJSON(rec, userHooksRecord{Agents: covered}, 0o600)
}

func userHooksRecordPath() (string, error) {
	dir, err := daemon.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, userHooksFile), nil
}

// userHooksCover reports whether a machine-wide hook handles tool's events: global mode
// wrote them for that agent.
func (app *App) userHooksCover(tool string) bool {
	path, err := userHooksRecordPath()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var rec userHooksRecord
	if json.Unmarshal(data, &rec) != nil {
		return false
	}
	return slices.Contains(rec.Agents, app.agentForTool(tool))
}

// agentForTool maps a hook's tool label to its agent's name.
func (app *App) agentForTool(tool string) string {
	return app.agents.NameForTool(tool)
}

// hookYields reports whether this hook invocation leaves the event to another: a
// machine-wide one (user) outside global mode — leftover from before the organization
// left it — or a repository's committed one in global mode, for an agent whose
// machine-wide hooks handle it.
func (app *App) hookYields(user bool, pol config.Policy, tool string) bool {
	if user {
		return !pol.Global()
	}
	return pol.Global() && app.userHooksCover(tool)
}

// managedHookFiles are where an organization deploys global mode's hooks as managed
// configuration, per agent: Claude Code's managed settings and Codex's system
// requirements. An agent whose managed file carries terma's hooks gets none from setup:
// both would run.
func (app *App) managedHookFiles(agent string) []string {
	a, ok := app.agents.Lookup(agent)
	if !ok {
		return nil
	}
	managed, ok := a.(agents.ManagedHooks)
	if !ok {
		return nil
	}
	return managed.ManagedHookFiles(app.managedRoot)
}

// managedHooksDeployed reports whether the organization deployed terma's hooks for agent
// as managed configuration.
func (app *App) managedHooksDeployed(agent string) bool {
	for _, f := range app.managedHookFiles(agent) {
		if data, err := os.ReadFile(f); err == nil && strings.Contains(string(data), " hook --user ") {
			return true
		}
	}
	return false
}

// writeManagedConfig writes global mode's hooks as managed configuration into dir, for
// an organization to deploy to every machine (MDM, configuration management), and a
// README saying where each file goes. terma is the path the hooks call it by on those
// machines ($HOME is expanded per user). It needs no sign-in.
func (app *App) writeManagedConfig(dir, terma string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	cmd := hookmgr.ManagedHookCommand(terma)
	managed := app.agents.With[agents.ManagedHooks]()
	var names, deploy []string
	files := map[string][]byte{}
	for _, a := range managed {
		name, data, err := a.ManagedConfig(cmd)
		if err != nil {
			return nil, err
		}
		files[name] = data
		names = append(names, a.DisplayName())
		deploy = append(deploy, "- `"+name+"` → "+a.ManagedDeploy())
	}
	files["README.md"] = []byte(`# terma global mode: managed hooks

Deploy these so every ` + strings.Join(names, " and ") + ` session on a machine is claimed, with no
trust step for anyone. Each developer still runs ` + "`terma setup`" + ` once: it points the
agents' exporters at the machine's relay, whose token is the machine's own.

` + strings.Join(deploy, "\n") + `

The hooks run terma as ` + "`" + terma + "`" + `; terma must be installed there for every user.
Where these are deployed, ` + "`terma setup`" + ` writes no per-user hooks of its own.
`)
	var out []string
	for name, data := range files {
		p := filepath.Join(dir, name)
		if err := config.WriteFileAtomic(p, data, 0o644); err != nil {
			return out, err
		}
		out = append(out, p)
	}
	slices.Sort(out)
	return out, nil
}
