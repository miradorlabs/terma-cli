package cli

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
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

// In global mode `terma setup` writes machine-wide hooks into each agent's user-level
// hooks file, and a repository's committed hooks step aside for them (hookYields): a
// repository's hooks may not run until each developer trusts them.

const userHooksFile = "user-hooks.json"

type userHooksRecord struct {
	Agents []string `json:"agents"`
}

// userHooksTrustSteps are what the developer must do before machine-wide hooks run.
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

func (app *App) applyUserHooks(selected []string, install bool) ([]string, error) {
	terma, err := app.hookExecutable()
	if err != nil {
		return nil, err
	}
	covered := app.userHookAgents(selected)
	var changed []string
	for _, a := range app.agents.With[agents.UserHooks]() {
		// Not where the organization's managed hooks run: both would fire.
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
	// Recorded whichever hooks run, so a repository's committed hooks step aside either way.
	return changed, config.WriteJSON(rec, userHooksRecord{Agents: covered}, 0o600)
}

func userHooksRecordPath() (string, error) {
	dir, err := daemon.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, userHooksFile), nil
}

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

func (app *App) agentForTool(tool string) string {
	return app.agents.NameForTool(tool)
}

// hookYields is true for a leftover machine-wide hook outside global mode, or a committed
// one in global mode whose agent has machine-wide hooks.
func (app *App) hookYields(user bool, pol config.Policy, tool string) bool {
	if user {
		return !pol.Global()
	}
	return pol.Global() && app.userHooksCover(tool)
}

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

func (app *App) managedHooksDeployed(agent string) bool {
	for _, f := range app.managedHookFiles(agent) {
		if data, err := os.ReadFile(f); err == nil && strings.Contains(string(data), " hook --user ") {
			return true
		}
	}
	return false
}

// writeManagedConfig needs no sign-in; terma is the path the hooks call it by on the
// deployed machines, with $HOME expanded per user.
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
