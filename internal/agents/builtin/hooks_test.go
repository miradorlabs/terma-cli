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

// hookCommand finds the event a committed hook entry runs. The files differ — JSON with
// the command under three different shapes — and the text `terma hook <event>` is what
// they all share, which is also all that reaches `terma hook` at run time.
var hookCommand = regexp.MustCompile(`terma hook ([a-z][a-z-]*)`)

// Event names are written twice: in hookmgr, into the files customers commit, and in
// each adapter's Events, where `terma hook <event>` looks them up. Nothing tied the two
// lists together, and the failure is silent on every side — a committed hook naming an
// event with no handler does nothing, for ever, in every repository that ran install.
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

// harness.SupportCatalog is what `terma harness` prints, and it names the agents again
// by hand. An adapter added to the registry and not to the catalog would be supported
// and unlisted; one renamed in a single place would be listed under a name install
// rejects.
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

// committedCommands plans a's committed hooks into a fresh repository and returns every
// command terma wrote there.
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

// Every hook entry terma commits must be silent on both streams and exit 0 on a machine
// without terma. Claude Code prints a hook's stderr in the transcript on a non-zero exit
// and hands SessionStart and PostToolUse stderr to the model; SessionStart's stdout
// becomes context as well. The agents run the command with `sh -c` and JSON on stdin.
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
				// A command may extend PATH before its guard (Codex's does); where that finds a
				// real terma, running the command would run it.
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

// A hooks file that exists and cannot be read is not an absent one: planned as a create,
// Apply would rename terma-only content over whatever the developer had there. A
// directory at the file's path fails to read on every platform.
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

// Global mode's machine-wide hooks: every agent's user-level file gets terma's entries by
// absolute path with --user, beside the developer's own; a second setup changes nothing,
// a moved terma is replaced in place, and removal leaves the developer's own.
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
