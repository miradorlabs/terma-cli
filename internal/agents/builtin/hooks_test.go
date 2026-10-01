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

// hookCommand finds the event a committed hook entry runs: `terma hook <event>` is the
// one text every hooks file shares.
var hookCommand = regexp.MustCompile(`terma hook ([a-z][a-z-]*)`)

// Every event a committed hooks file names has a handler in its own adapter; a missing
// one fails silently in every repository that ran install.
func TestEveryCommittedHookHasAHandler(t *testing.T) {
	handlers := reg.Handlers()
	for _, a := range reg.All() {
		if a.HooksPath() == "" {
			continue
		}
		plan, err := a.Plan(t.TempDir(), true)
		if err != nil {
			t.Fatalf("%s: %v", a.Name(), err)
		}
		own := a.Events()
		found := 0
		for _, change := range plan.Changes {
			for _, m := range hookCommand.FindAllStringSubmatch(string(change.After), -1) {
				found++
				event := m[1]
				if _, ok := handlers[event]; !ok {
					t.Errorf("%s commits `terma hook %s`, which no adapter handles", a.Name(), event)
				} else if _, ok := own[event]; !ok {
					t.Errorf("%s commits `terma hook %s`, which belongs to another adapter", a.Name(), event)
				}
			}
		}
		if found == 0 {
			t.Errorf("%s writes %s with no `terma hook` command in it", a.Name(), a.HooksPath())
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

func committedCommands(t *testing.T, a agents.Agent) []string {
	t.Helper()
	plan, err := a.Plan(t.TempDir(), true)
	if err != nil {
		t.Fatalf("%s: %v", a.Name(), err)
	}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if s, ok := x.(string); ok && k == "command" && strings.Contains(s, "terma hook") {
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
	for _, change := range plan.Changes {
		var doc any
		if json.Unmarshal(change.After, &doc) == nil {
			walk(doc)
		}
	}
	return out
}

// Every committed hook entry is silent on both streams and exits 0 without terma: an
// agent may hand a hook's output to the model.
func TestCommittedHookCommandsAreInertWithoutTerma(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}
	for _, a := range reg.All() {
		if a.HooksPath() == "" {
			continue
		}
		for _, command := range committedCommands(t, a) {
			t.Run(a.Name()+"/"+command, func(t *testing.T) {
				dir := t.TempDir()
				// A command may extend PATH before its guard; if that finds a real terma, skip.
				if i := strings.Index(command, "; command -v terma"); i >= 0 {
					probe := exec.Command("sh", "-c", command[:i+2]+"command -v terma")
					probe.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir}
					if probe.Run() == nil {
						t.Skip("a terma is installed where this command looks for one")
					}
				}
				cmd := exec.Command("sh", "-c", command)
				cmd.Dir = dir
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
	for _, a := range reg.All() {
		if a.HooksPath() == "" || strings.HasSuffix(a.HooksPath(), "/") {
			continue
		}
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(a.HooksPath())), 0o755); err != nil {
			t.Fatal(err)
		}
		if plan, err := a.Plan(root, true); err == nil {
			t.Errorf("%s planned %d change(s) over a file that could not be read", a.Name(), len(plan.Changes))
		}
	}
}

// What install commits, uninstall recognizes as terma's and removes, and a second install
// changes nothing.
func TestCommittedHooksRoundTrip(t *testing.T) {
	for _, a := range reg.All() {
		if a.HooksPath() == "" {
			continue
		}
		root := t.TempDir()
		plan, err := a.Plan(root, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := hookmgr.Apply(root, plan); err != nil {
			t.Fatalf("%s: %v", a.Name(), err)
		}
		if again, err := a.Plan(root, true); err != nil || !again.Empty() {
			t.Errorf("%s: a second install plans %+v (%v)", a.Name(), again.Changes, err)
		}
		if !agents.Wired(root, a) {
			t.Errorf("%s: installed hooks do not read as wired", a.Name())
		}
		remove, err := a.Plan(root, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := hookmgr.Apply(root, remove); err != nil {
			t.Fatalf("%s: %v", a.Name(), err)
		}
		if agents.Wired(root, a) {
			t.Errorf("%s: uninstall left terma's hooks in place", a.Name())
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
