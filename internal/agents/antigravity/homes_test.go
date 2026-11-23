package antigravity

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
)

// isolatedHome is a home folder of the test's own, with T3CODE_HOME unset and no agy on
// PATH unless the test put one there first.
func isolatedHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// os.UserHomeDir reads USERPROFILE on Windows and HOME elsewhere.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("T3CODE_HOME", "")
	return home
}

// t3Home makes the Antigravity home T3 Code keeps for one instance and account below the
// T3 home t3, as T3 Code does: <t3>/userdata/providers/<instance>/<id>/antigravity-acp.
func t3Home(t *testing.T, t3, instance, id string) string {
	t.Helper()
	home := filepath.Join(t3, "userdata", "providers", instance, id)
	if err := os.MkdirAll(filepath.Join(home, t3AppDataDir), 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

// agy's own home comes first; T3 Code's are found below $T3CODE_HOME when it is absolute,
// else below ~/.t3, by their antigravity-acp folder whatever the instance is named.
func TestAntigravityHomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		// env is T3CODE_HOME: "<tmp>" for a folder of the test's own.
		env string
		// under is where T3 Code's folders are made: "~" for ~/.t3, "<tmp>" for env's.
		under string
		want  []string
	}{
		{"none", "", "", nil},
		{"home", "", "~", []string{"antigravity/a1", "antigravity/b2", "work-neon/c3"}},
		{"relative", "relative/t3", "~", []string{"antigravity/a1", "antigravity/b2", "work-neon/c3"}},
		{"env", "<tmp>", "<tmp>", []string{"antigravity/a1", "antigravity/b2", "work-neon/c3"}},
		{"env-not-home", "<tmp>", "~", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolatedHome(t)
			env := tc.env
			if env == "<tmp>" {
				env = t.TempDir()
			}
			t.Setenv("T3CODE_HOME", env)
			t3 := ""
			switch tc.under {
			case "~":
				t3 = filepath.Join(home, ".t3")
			case "<tmp>":
				t3 = env
			}
			if t3 != "" {
				// Made out of order, to be found sorted.
				for _, h := range []string{"work-neon/c3", "antigravity/b2", "antigravity/a1"} {
					instance, id, _ := strings.Cut(h, "/")
					t3Home(t, t3, instance, id)
				}
				// Another provider's folder, a file where an id would be and a home's
				// app data folder that is a file hold no Antigravity home.
				hookruntest.WriteFile(t, t3, "userdata/providers/codex/d4/sessions/x.json", "{}")
				hookruntest.WriteFile(t, t3, "userdata/providers/antigravity/notes.txt", "x")
				hookruntest.WriteFile(t, t3, "userdata/providers/antigravity/e5/"+t3AppDataDir, "x")
			}
			got, err := homes()
			if err != nil {
				t.Fatal(err)
			}
			want := []string{filepath.Join(home, ".gemini")}
			for _, h := range tc.want {
				want = append(want, filepath.Join(t3, "userdata", "providers", filepath.FromSlash(h)))
			}
			if !slices.Equal(got, want) {
				t.Fatalf("homes = %q\nwant %q", got, want)
			}
			paths, err := Agent{}.UserHooksPaths()
			if err != nil || len(paths) != len(want) {
				t.Fatalf("UserHooksPaths = %q, %v", paths, err)
			}
			for i, h := range want {
				if paths[i] != filepath.Join(h, "config", "hooks.json") {
					t.Errorf("UserHooksPaths[%d] = %q, want %q", i, paths[i], filepath.Join(h, "config", "hooks.json"))
				}
			}
			clients, err := clientHomes()
			if err != nil || !slices.Equal(clients, want[1:]) {
				t.Errorf("clientHomes = %q, %v; want %q", clients, err, want[1:])
			}
		})
	}
}

// A machine with T3 Code's Antigravity and no agy on PATH has Antigravity to set up; one
// with neither has none.
func TestAntigravityInstalledWithAClientHome(t *testing.T) {
	home := isolatedHome(t)
	t.Setenv("PATH", t.TempDir())
	ctx := context.Background()
	if (Agent{}).Installed(ctx) {
		t.Fatal("installed with no agy and no client home")
	}
	t3Home(t, filepath.Join(home, ".t3"), "antigravity", "a1")
	if !(Agent{}).Installed(ctx) {
		t.Fatal("not installed with a client home")
	}
	if v := (Agent{}).DetectedVersion(ctx); v != "" {
		t.Errorf("DetectedVersion with no agy = %q", v)
	}
}

// guardHook is a named hook of the developer's beside terma's, as the fleet's secret guard is.
const guardHook = `{
  "fleet-secret-guard": {
    "PreToolUse": [{"matcher": "", "hooks": [{"type": "command", "command": "/opt/fleet/guard", "timeout": 5}]}]
  }
}
`

// member is the named hook name in the hooks file at path, as JSON; nil when absent.
func member(t *testing.T, path, name string) any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, data)
	}
	return top[name]
}

// refreshHome is a home with agy's own home and three of T3 Code's, no agy on PATH, and a
// hooks file written into a home's config folder.
type refreshHome struct {
	gemini, t3a, t3b, t3c string
}

func newRefreshHome(t *testing.T) refreshHome {
	t.Helper()
	home := isolatedHome(t)
	t.Setenv("PATH", t.TempDir())
	t3 := filepath.Join(home, ".t3")
	return refreshHome{
		gemini: filepath.Join(home, ".gemini"),
		t3a:    t3Home(t, t3, "antigravity", "a1"),
		t3b:    t3Home(t, t3, "antigravity", "b2"),
		t3c:    t3Home(t, t3, "work-neon", "c3"),
	}
}

func hooksAt(home string) string { return filepath.Join(home, "config", "hooks.json") }

// installAt writes terma's hook into home's file as setup does, running terma.
func installAt(t *testing.T, home, terma string) {
	t.Helper()
	if _, err := installHooks(hooksAt(home), terma); err != nil {
		t.Fatal(err)
	}
}

// A refresh points terma's hook at this terma in every home that has it, and puts it into a
// home that names no terma hook at all, keeping the developer's other named hooks there; a
// home whose terma hook is switched off stays as it is.
func TestAntigravityRefreshCoversEveryHome(t *testing.T) {
	h := newRefreshHome(t)
	exe, err := procinfo.AbsExecutable()
	if err != nil {
		t.Fatal(err)
	}
	installAt(t, h.gemini, "/old/terma")
	installAt(t, h.t3a, "/old/terma")
	hookruntest.WriteFile(t, filepath.Join(h.t3b, "config"), "hooks.json", guardHook)
	off := `{"terma": {"enabled": false}}` + "\n"
	hookruntest.WriteFile(t, filepath.Join(h.t3c, "config"), "hooks.json", off)
	guard := member(t, hooksAt(h.t3b), "fleet-secret-guard")

	changed, ok, err := Agent{}.refreshHooksFile()
	if err != nil || !ok {
		t.Fatalf("refresh = %q, %v, %v", changed, ok, err)
	}
	if want := strings.Join([]string{hooksAt(h.gemini), hooksAt(h.t3a), hooksAt(h.t3b)}, ", "); changed != want {
		t.Errorf("refresh named %q\nwant %q", changed, want)
	}
	for _, home := range []string{h.gemini, h.t3a, h.t3b} {
		plan, err := planHooks(filepath.Join(home, "config"), hookmgr.UserHookCommand(exe), modeInstall)
		if err != nil || !plan.Empty() {
			t.Errorf("%s: terma's hook is not this terma's: %+v, %v\n%s", home, plan, err, hookruntest.ReadFile(t, filepath.Join(home, "config"), "hooks.json"))
		}
	}
	if got := member(t, hooksAt(h.t3b), "fleet-secret-guard"); !jsonEqual(t, got, guard) {
		t.Errorf("the developer's named hook changed: %v, want %v", got, guard)
	}
	if got := hookruntest.ReadFile(t, filepath.Join(h.t3c, "config"), "hooks.json"); got != off {
		t.Errorf("a switched-off terma hook was written:\n%s", got)
	}
	if _, ok, err := (Agent{}).refreshHooksFile(); ok || err != nil {
		t.Errorf("a second refresh = %v, %v", ok, err)
	}
}

// With no home carrying terma's hook, a refresh writes nothing anywhere: no file and no
// config folder.
func TestAntigravityRefreshWithoutTermasHookWritesNothing(t *testing.T) {
	h := newRefreshHome(t)
	hookruntest.WriteFile(t, filepath.Join(h.t3b, "config"), "hooks.json", guardHook)
	if changed, ok, err := (Agent{}).refreshHooksFile(); ok || err != nil || changed != "" {
		t.Fatalf("refresh = %q, %v, %v", changed, ok, err)
	}
	for _, home := range []string{h.gemini, h.t3a, h.t3c} {
		if _, err := os.Stat(filepath.Join(home, "config")); !os.IsNotExist(err) {
			t.Errorf("%s: config folder made: %v", home, err)
		}
	}
	if got := hookruntest.ReadFile(t, filepath.Join(h.t3b, "config"), "hooks.json"); got != guardHook {
		t.Errorf("the developer's file changed:\n%s", got)
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	x, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(x) == string(y)
}
