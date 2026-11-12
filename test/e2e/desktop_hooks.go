package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A desktop app's agent's hooks, on the desktop profile's one turn: the hooks probe's recorder
// is registered for every event the agent knows, where a person's own hooks are, and what the
// agent hands each that fires is recorded as the hooks probe records it. The probe's own runs
// (tool calls, subagents, compaction, an interactive session) are the agent's own snapshot's.

// desktopHooks registers the hooks probe's recorder, saving into rec, for every hook event the
// agent knows (events), in the agent's user-level hooks configuration at path, as a person's
// own hooks are: Claude Code's settings.json, Codex's hooks.json.
func desktopHooks(t *testing.T, agent string, events []string, path, rec string) {
	t.Helper()
	hooks := map[string]any{}
	for _, e := range events {
		cmd, err := HookRecorderCommand(rec, e, hookMatchAll)
		if err != nil {
			t.Fatal(err)
		}
		group := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd, "timeout": 30}}}
		if agent == "claude" {
			group["matcher"] = "*"
		}
		hooks[e] = []any{group}
	}
	var doc map[string]any
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &doc)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	doc["hooks"] = hooks
	data, _ := json.MarshalIndent(doc, "", "  ")
	writeFiles(t, map[string]string{path: string(data) + "\n"})
}

// recordDesktopHooks records what the recorder in rec saw the app's agent hand its hooks on
// the one turn: the hooks section, its examples scrubbed as the hooks probe's are. started is
// the environment the agent was started with, root the folder a path is kept under, trust how
// the hooks were trusted where the agent gates them on trust ("trusted"; "" where it does not).
// An event the turn did not fire says so; whether an event takes a matcher is not measured.
func recordDesktopHooks(t *testing.T, b Binary, events []string, source, rec string, started []string, root, trust string) {
	t.Helper()
	calls := HookCallsIn(rec)
	probe := HooksProbeOf(events, source, calls, started, root)
	probe.Method = 0
	probe.Trust = trust
	probe.NotFired = map[string]string{}
	for _, e := range probe.Supported {
		if s := probe.Events[e]; s == nil {
			probe.NotFired[e] = "the desktop profile runs one first turn, not the hooks probe: the turn did not fire it"
		} else {
			s.Matcher = nil
		}
	}
	t.Logf("%s %s: %d hook runs of %d events", b.Harness, b.Version, len(calls), len(probe.Events))
	RecordProbe(t, b, "hooks", probe)
}

// trustCodexDesktopHooks trusts every hook the Codex at codex lists for cwd, in the Codex home
// at home, as the app's Settings → Hooks does: each hook's key and hash, from the bundled
// app-server's hooks/list, into config.toml's [hooks.state."<key>"] trusted_hash. It returns
// "trusted" where the build gates hooks on trust, "" where it does not.
func trustCodexDesktopHooks(t *testing.T, codex, home, cwd string) string {
	t.Helper()
	env := append(os.Environ(), "CODEX_HOME="+home)
	a, err := launchAppServer(t, codex, []string{"app-server"}, cwd, env, "terma-e2e")
	if err != nil {
		t.Fatalf("start the bundled app-server to trust the hooks: %v", err)
	}
	defer a.Close()
	res, err := a.tryCall("hooks/list", map[string]any{"cwds": []string{cwd}})
	if err != nil {
		t.Fatalf("hooks/list: %v", err)
	}
	hashes := map[string]string{}
	for _, h := range listedHooks(res) {
		if h.Status != "" && !h.Managed && !strings.EqualFold(h.Status, "trusted") {
			hashes[h.Key] = h.Hash
		}
	}
	gated := slices.ContainsFunc(listedHooks(res), func(h codexListedHook) bool { return h.Status != "" })
	if len(hashes) > 0 {
		cfg := filepath.Join(home, "config.toml")
		existing, _ := os.ReadFile(cfg)
		if err := os.WriteFile(cfg, []byte(withCodexHookTrust(string(existing), hashes)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if gated {
		return "trusted"
	}
	return ""
}

// writeFiles writes each file, its folder made.
func writeFiles(t *testing.T, files map[string]string) {
	t.Helper()
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
