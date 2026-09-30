package cmd

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
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
func userHookAgents(agents []string) []string {
	var out []string
	for _, a := range []string{"claude", "codex", "cursor"} {
		if slices.Contains(agents, a) || a == "codex" && slices.Contains(agents, codexDesktopAgent) {
			out = append(out, a)
		}
	}
	return out
}

// userHooksDir is where an agent keeps its user-level hooks file, and the file's name.
func userHooksDir(agent string) (dir, file string, err error) {
	switch agent {
	case "claude":
		path, err := harness.Claude{}.ConfigPath()
		return filepath.Dir(path), "settings.json", err
	case "codex":
		path, err := harness.Codex{}.ConfigPath()
		return filepath.Dir(path), "hooks.json", err
	case "cursor":
		home, err := os.UserHomeDir()
		return filepath.Join(home, ".cursor"), "hooks.json", err
	}
	return "", "", errors.New("no user-level hooks for " + agent)
}

func planUserHooks(agent, dir, terma string, install bool) (hookmgr.Plan, error) {
	cmd := hookmgr.UserHookCommand(terma)
	switch agent {
	case "claude":
		return hookmgr.PlanClaudeUserHooks(dir, cmd, install)
	case "codex":
		return hookmgr.PlanCodexUserHooks(dir, cmd, install)
	default:
		return hookmgr.PlanCursorUserHooks(dir, cmd, install)
	}
}

// applyUserHooks writes terma's machine-wide hooks for the developer's agents (install)
// or removes every one terma wrote (not install), and records which agents have them.
// It reports the files it changed.
func applyUserHooks(agents []string, install bool) ([]string, error) {
	terma, err := hookExecutable()
	if err != nil {
		return nil, err
	}
	targets := userHookAgents(agents)
	if !install {
		targets = []string{"claude", "codex", "cursor"}
	}
	var changed []string
	for _, a := range targets {
		dir, file, err := userHooksDir(a)
		if err != nil {
			return changed, err
		}
		if !install {
			if _, err := os.Stat(filepath.Join(dir, file)); errors.Is(err, fs.ErrNotExist) {
				continue
			}
		}
		plan, err := planUserHooks(a, dir, terma, install)
		if err != nil {
			return changed, err
		}
		if plan.Empty() {
			continue
		}
		if install {
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
	return changed, config.WriteJSON(rec, userHooksRecord{Agents: userHookAgents(agents)}, 0o600)
}

func userHooksRecordPath() (string, error) {
	dir, err := relayDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, userHooksFile), nil
}

// userHooksCover reports whether a machine-wide hook handles tool's events: global mode
// wrote them for that agent.
func userHooksCover(tool string) bool {
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
	return slices.Contains(rec.Agents, agentForTool(tool))
}

// agentForTool maps a hook's tool label to its agent's name.
func agentForTool(tool string) string {
	if tool == "claude-code" {
		return "claude"
	}
	return tool
}

// hookYields reports whether this hook invocation leaves the event to another: a
// machine-wide one (user) outside global mode — leftover from before the organization
// left it — or a repository's committed one in global mode, for an agent whose
// machine-wide hooks handle it.
func hookYields(user bool, pol config.Policy, tool string) bool {
	if user {
		return !pol.Global()
	}
	return pol.Global() && userHooksCover(tool)
}

// hookExecutable is the terma machine-wide hooks and git's global hooks call: this one,
// by the path it was started as. A variable so a test (whose executable is the test
// binary) can name a built terma.
var hookExecutable = relayServiceExecutable
