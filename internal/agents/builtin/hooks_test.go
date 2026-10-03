package builtin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// userCommand is a machine-wide hook entry's command, run by the full path setup wrote.
var userCommand = hookmgr.UserHookCommand("/nonexistent/terma")

// hookCommand finds the event a hook entry runs: `hook --user <event>` is the one text
// every machine-wide hooks file shares.
var hookCommand = regexp.MustCompile(`hook --user ([a-z][a-z-]*)`)

// userPlan is what setup writes into an empty machine-wide hooks directory for a.
func userPlan(t *testing.T, a agents.UserHooks) hookmgr.Plan {
	t.Helper()
	plan, err := a.PlanUserHooks(t.TempDir(), userCommand, true)
	if err != nil {
		t.Fatalf("%s: %v", a.Name(), err)
	}
	return plan
}

// Every event a machine-wide hooks file names has a handler in its own adapter; a missing
// one fails silently on every machine that ran setup.
func TestEveryMachineWideHookHasAHandler(t *testing.T) {
	handlers := reg.Handlers()
	for _, a := range reg.With[agents.UserHooks]() {
		own := a.Events()
		found := 0
		for _, change := range userPlan(t, a).Changes {
			for _, m := range hookCommand.FindAllStringSubmatch(string(change.After), -1) {
				found++
				event := m[1]
				if _, ok := handlers[event]; !ok {
					t.Errorf("%s writes `terma hook %s`, which no adapter handles", a.Name(), event)
				} else if _, ok := own[event]; !ok {
					t.Errorf("%s writes `terma hook %s`, which belongs to another adapter", a.Name(), event)
				}
			}
		}
		if found == 0 {
			t.Errorf("%s writes no `terma hook` command", a.Name())
		}
	}
}

// The support catalog lists exactly the registry's supported agents, by the same names.
func TestSupportCatalogMatchesTheRegistry(t *testing.T) {
	listed := map[string]string{}
	for _, agent := range reg.SupportCatalog() {
		listed[agent.Name] = agent.DisplayName
	}
	for _, a := range reg.All() {
		name, ok := listed[a.Name()]
		if !ok {
			t.Errorf("adapter %q is missing from harness.SupportCatalog", a.Name())
			continue
		}
		if name != a.DisplayName() {
			t.Errorf("%s: the catalog calls it %q, the adapter %q", a.Name(), name, a.DisplayName())
		}
		delete(listed, a.Name())
	}
	for name := range listed {
		t.Errorf("harness.SupportCatalog lists %q, which is not an adapter", name)
	}
}

func userCommands(t *testing.T, a agents.UserHooks) []string {
	t.Helper()
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if s, ok := x.(string); ok && k == "command" && strings.Contains(s, "hook --user") {
					out = append(out, s)
				}
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	for _, change := range userPlan(t, a).Changes {
		var doc any
		if json.Unmarshal(change.After, &doc) == nil {
			walk(doc)
		}
	}
	return out
}

// Every machine-wide hook entry is silent on both streams and exits 0 once terma is gone:
// an agent may hand a hook's output to the model.
func TestMachineWideHookCommandsAreInertWithoutTerma(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}
	for _, a := range reg.With[agents.UserHooks]() {
		for _, command := range userCommands(t, a) {
			t.Run(a.Name()+"/"+command, func(t *testing.T) {
				cmd := exec.Command("sh", "-c", command)
				cmd.Dir = t.TempDir()
				cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + cmd.Dir}
				cmd.Stdin = strings.NewReader(`{"session_id":"s1","hook_event_name":"SessionStart","cwd":"` + cmd.Dir + `"}`)
				var stdout, stderr strings.Builder
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				if err := cmd.Run(); err != nil {
					t.Fatalf("exit without terma: %v\nstderr: %s", err, stderr.String())
				}
				if stdout.Len() != 0 || stderr.Len() != 0 {
					t.Fatalf("output without terma: stdout %q, stderr %q", stdout.String(), stderr.String())
				}
			})
		}
	}
}

// An unreadable hooks file is an error, never a create that Apply would rename over the
// developer's file.
func TestPlannersRefuseAFileTheyCannotRead(t *testing.T) {
	for _, a := range reg.With[agents.UserHooks]() {
		path, err := a.UserHooksPath()
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, filepath.Base(path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if plan, err := a.PlanUserHooks(dir, userCommand, true); err == nil {
			t.Errorf("%s planned %d change(s) over a file that could not be read", a.Name(), len(plan.Changes))
		}
	}
}

// Machine-wide hooks are written by absolute path beside the developer's own, idempotent,
// replaced in place when terma moves, and removed leaving the developer's own.
func TestUserHooksInstallIdempotentlyAndRemoveCleanly(t *testing.T) {
	mine := map[string]string{
		"claude": `{"env":{"A":"1"},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}`,
		"codex":  `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}`,
		"cursor": `{"version":1,"hooks":{"stop":[{"command":"say done"}]}}`,
	}
	for _, a := range reg.With[agents.UserHooks]() {
		t.Run(a.Name(), func(t *testing.T) {
			own, ok := mine[a.Name()]
			if !ok {
				t.Fatalf("no developer's own hooks file for %s in this test", a.Name())
			}
			userPath, err := a.UserHooksPath()
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			path := filepath.Join(dir, filepath.Base(userPath))
			if err := os.WriteFile(path, []byte(own), 0o600); err != nil {
				t.Fatal(err)
			}
			apply := func(cmd func(string) string, install bool) hookmgr.Plan {
				t.Helper()
				p, err := a.PlanUserHooks(dir, cmd, install)
				if err != nil {
					t.Fatal(err)
				}
				if err := hookmgr.Apply(dir, p); err != nil {
					t.Fatal(err)
				}
				return p
			}
			first := hookmgr.UserHookCommand("/opt/it's terma/bin/terma")
			apply(first, true)
			data, _ := os.ReadFile(path)
			if !strings.Contains(string(data), `hook --user`) || !strings.Contains(string(data), "say done") {
				t.Fatalf("after install:\n%s", data)
			}
			if p := apply(first, true); !p.Empty() {
				t.Fatalf("a second install changed %v", p.Changes)
			}
			moved := hookmgr.UserHookCommand("/home/dev/.local/bin/terma")
			apply(moved, true)
			data, _ = os.ReadFile(path)
			if strings.Contains(string(data), "/opt/it") || strings.Count(string(data), "hook --user") != strings.Count(string(data), "/home/dev/.local/bin/terma' hook") {
				t.Fatalf("a moved terma was not replaced in place:\n%s", data)
			}
			apply(moved, false)
			data, _ = os.ReadFile(path)
			if strings.Contains(string(data), "hook --user") || !strings.Contains(string(data), "say done") {
				t.Fatalf("after removal:\n%s", data)
			}
		})
	}
}
