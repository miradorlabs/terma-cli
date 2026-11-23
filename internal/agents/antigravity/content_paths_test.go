package antigravity

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
)

// pathRig is a collected checkout, a second collected one, an excluded one, an excluded clone
// nested in the collected one, and a folder in no repository.
type pathRig struct {
	collected, other, excluded, nested, none string
	pol                                      config.Policy
}

func newPathRig(t *testing.T) pathRig {
	t.Helper()
	g := pathRig{collected: hookruntest.InitRepo(t), other: hookruntest.InitRepo(t), excluded: hookruntest.InitRepo(t)}
	g.nested = filepath.Join(g.collected, "vendor", "nested")
	if err := os.MkdirAll(g.nested, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://github.com/acme/nested-excluded.git"}} {
		if _, err := gitx.Git(context.Background(), g.nested, args...); err != nil {
			t.Fatal(err)
		}
	}
	none, _ := filepath.EvalSymlinks(t.TempDir())
	g.none = none
	for _, dir := range []string{g.collected, g.other, g.excluded, g.nested, g.none} {
		hookruntest.WriteFile(t, dir, "file.txt", "made-up file\n")
	}
	g.pol = config.DefaultPolicy()
	g.pol.Repositories = append(hookruntest.Admitting(g.collected).Repositories, hookruntest.Admitting(g.other).Repositories...)
	return g
}

// Which checkout owns a path: worked out from the file, its links resolved, the nearest
// checkout that holds it.
func TestAntigravityPathOwner(t *testing.T) {
	g := newPathRig(t)
	link := filepath.Join(g.collected, "into-excluded")
	if err := os.Symlink(g.excluded, link); err != nil {
		t.Skip("no symlinks here")
	}
	for _, tc := range []struct {
		name, cwd, path string
		want            antigravityOwner
	}{
		{"the turn's checkout", "", filepath.Join(g.collected, "file.txt"), ownerCollected},
		{"another collected checkout", "", filepath.Join(g.other, "file.txt"), ownerCollected},
		{"an excluded checkout", "", filepath.Join(g.excluded, "file.txt"), ownerExcluded},
		{"through a link into an excluded checkout", "", filepath.Join(link, "file.txt"), ownerExcluded},
		{"an excluded clone nested in a collected one", "", filepath.Join(g.nested, "file.txt"), ownerExcluded},
		{"no repository", "", filepath.Join(g.none, "file.txt"), ownerNone},
		{"a file not there yet", "", filepath.Join(g.excluded, "new", "deeper", "file.txt"), ownerExcluded},
		{"relative to the cwd", g.collected, "file.txt", ownerCollected},
		{"relative, climbing into an excluded checkout", g.collected, filepath.Join("..", filepath.Base(g.excluded), "file.txt"), ownerExcluded},
		{"relative with no cwd", "", "file.txt", ownerUnresolved},
		{"a missing path that climbs", "", g.collected + string(filepath.Separator) + filepath.Join("missing", "x") + string(filepath.Separator) + ".." + string(filepath.Separator) + "y", ownerUnresolved},
	} {
		if got := antigravityPathOwner(g.pol, tc.cwd, tc.path); got != tc.want {
			t.Errorf("%s: owner %d, want %d", tc.name, got, tc.want)
		}
	}
	// Global mode collects every checkout.
	if got := antigravityPathOwner(config.Policy{Mode: config.ModeGlobal}, "", filepath.Join(g.excluded, "file.txt")); got != ownerCollected {
		t.Errorf("global mode: owner %d", got)
	}
}

// A call is refused when its arguments name a file in an excluded checkout, under any key
// and for any tool; a file in no repository is shown or refused by the package's answer, both
// ways.
func TestAntigravityRefusesAFileOfAnExcludedCheckout(t *testing.T) {
	g := newPathRig(t)
	refuses := func(tool, key, path string) bool {
		in := &antigravityHookInput{}
		if err := jsonUnmarshalInto(`{"name":"`+tool+`","args":{"`+key+`":"`+hookruntest.InJSON(path)+`","Other":1}}`, in); err != nil {
			t.Fatal(err)
		}
		return antigravityRefuses(hookrun.Env{Policy: g.pol, Cwd: g.collected}, in)
	}
	for _, tc := range []struct {
		tool, key, path string
		want            bool
	}{
		{"read_file", "Path", filepath.Join(g.collected, "file.txt"), false},
		{"read_file", "Path", filepath.Join(g.other, "file.txt"), false},
		{"read_file", "Path", filepath.Join(g.excluded, "file.txt"), true},
		{"view_file", "AbsolutePath", filepath.Join(g.excluded, "file.txt"), true},
		{"some_mcp_tool", "file_path", filepath.Join(g.nested, "file.txt"), true},
		{"read_file", "Path", filepath.Join(g.none, "file.txt"), false},
		{"run_command", "CommandLine", "cat " + filepath.Join(g.excluded, "file.txt"), false},
	} {
		if got := refuses(tc.tool, tc.key, tc.path); got != tc.want {
			t.Errorf("%s %s=%s: refused %v, want %v", tc.tool, tc.key, tc.path, got, tc.want)
		}
	}
	defer func(was bool) { antigravityShowsFilesInNoRepository = was }(antigravityShowsFilesInNoRepository)
	antigravityShowsFilesInNoRepository = false
	if !refuses("read_file", "Path", filepath.Join(g.none, "file.txt")) {
		t.Error("a file in no repository was shown with the answer withholding it")
	}
	if refuses("read_file", "Path", filepath.Join(g.collected, "file.txt")) {
		t.Error("withholding files in no repository refused a collected one")
	}
}

// Content goes only when every workspace is in a checkout the policy collects: a second
// workspace in an excluded checkout, or outside git, stops it; global mode collects both.
func TestAntigravityWorkspacesMustAllBeCollected(t *testing.T) {
	g := newPathRig(t)
	ctx := context.Background()
	collected := func(pol config.Policy, cwd string, workspaces ...string) bool {
		return antigravityWorkspacesCollected(ctx, hookrun.Env{Policy: pol, Cwd: cwd}, &antigravityHookInput{WorkspacePaths: workspaces})
	}
	for _, tc := range []struct {
		name       string
		cwd        string
		workspaces []string
		want       bool
	}{
		{"one collected workspace", g.collected, []string{g.collected}, true},
		{"two collected", g.collected, []string{g.collected, g.other}, true},
		{"resumed from an excluded checkout", g.collected, []string{g.collected, g.excluded}, false},
		{"resumed from outside git", g.collected, []string{g.collected, g.none}, false},
		{"no list: the cwd", g.collected, nil, true},
		{"no list, the cwd excluded", g.excluded, nil, false},
		{"a relative entry", g.collected, []string{"."}, false},
	} {
		if got := collected(g.pol, tc.cwd, tc.workspaces...); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if !collected(config.Policy{Mode: config.ModeGlobal}, g.collected, g.collected, g.none) {
		t.Error("global mode withheld a workspace outside git")
	}
}
