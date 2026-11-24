package antigravity

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
		// The folder a tool lists, searches or runs in.
		{"list_dir", "DirectoryPath", g.excluded, true},
		{"grep_search", "SearchPath", g.excluded, true},
		{"find_by_name", "SearchDirectory", g.nested, true},
		{"run_command", "Cwd", g.excluded, true},
		{"run_command", "Cwd", g.collected, false},
		// A folder in no repository is refused whatever the answer for files: a search of one
		// above several repositories returns their files.
		{"grep_search", "SearchPath", g.none, true},
		{"list_dir", "DirectoryPath", g.none, true},
		{"run_command", "Cwd", g.none, true},
	} {
		if got := refuses(tc.tool, tc.key, tc.path); got != tc.want {
			t.Errorf("%s %s=%s: refused %v, want %v", tc.tool, tc.key, tc.path, got, tc.want)
		}
	}
	// Global mode collects a session outside git, so a folder outside git is searched freely.
	global := &antigravityHookInput{}
	if err := jsonUnmarshalInto(`{"name":"grep_search","args":{"SearchPath":"`+hookruntest.InJSON(g.none)+`"}}`, global); err != nil {
		t.Fatal(err)
	}
	if antigravityRefuses(hookrun.Env{Policy: config.Policy{Mode: config.ModeGlobal}, Cwd: g.collected}, global) {
		t.Error("global mode refused a search of a folder outside git")
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
	collected := func(pol config.Policy, cwd string, workspaces ...string) bool {
		return antigravityWorkspacesCollected(hookrun.Env{Policy: pol, Cwd: cwd}, &antigravityHookInput{WorkspacePaths: workspaces})
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
		// A removed clone nested in a collected checkout is not judged by the checkout above it.
		{"a workspace that does not exist", g.collected, []string{g.collected, filepath.Join(g.collected, "vendor", "removed")}, false},
		{"no list, the cwd removed", filepath.Join(g.collected, "removed"), nil, false},
		{"a file", g.collected, []string{filepath.Join(g.collected, "file.txt")}, false},
	} {
		if got := collected(g.pol, tc.cwd, tc.workspaces...); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if !collected(config.Policy{Mode: config.ModeGlobal}, g.collected, g.collected, g.none) {
		t.Error("global mode withheld a workspace outside git")
	}
	// A workspace outside git alone: not collected in repository mode, collected in global
	// mode, as a session outside git is.
	if collected(g.pol, g.none, g.none) {
		t.Error("repository mode collected a workspace outside git")
	}
	if !collected(config.Policy{Mode: config.ModeGlobal}, g.none, g.none) {
		t.Error("global mode withheld a lone workspace outside git")
	}
	// A link in a collected checkout to a folder of an excluded one is judged where it leads.
	sub := filepath.Join(g.excluded, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(g.collected, "into-excluded-sub")
	if err := os.Symlink(sub, link); err != nil {
		t.Skip("no symlinks here")
	}
	if collected(g.pol, link, link) {
		t.Error("a workspace linked into an excluded checkout was collected")
	}
}

// Where a path's checkout cannot be trusted, it is unresolved: with GIT_DIR in the hook's
// environment (no workspace is collected then, global mode or not), a path that starts with
// "~", and a dangling link. A linked worktree is its main checkout's; a submodule its own.
func TestAntigravityPathOwnerFailsClosed(t *testing.T) {
	g := newPathRig(t)
	dangling := filepath.Join(g.collected, "dangling")
	if err := os.Symlink(filepath.Join(g.collected, "nowhere"), dangling); err != nil {
		t.Skip("no symlinks here")
	}
	ctx := context.Background()
	for _, args := range [][]string{{"commit", "-q", "--allow-empty", "-m", "made-up"}, {"worktree", "add", "-q", filepath.Join(t.TempDir(), "wt")}} {
		if _, err := gitx.Git(ctx, g.collected, args...); err != nil {
			t.Fatal(err)
		}
	}
	worktree, err := gitx.Git(ctx, g.collected, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	var linked string
	for line := range strings.Lines(worktree) {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "worktree "); ok && p != g.collected {
			linked = p
		}
	}
	if linked == "" {
		t.Fatalf("no linked worktree in %q", worktree)
	}
	// A submodule: its .git a file naming its git directory in the superproject's modules.
	sub := filepath.Join(g.collected, "sub")
	modules := filepath.Join(g.collected, ".git", "modules", "sub")
	hookruntest.WriteFile(t, modules, "HEAD", "ref: refs/heads/main\n")
	hookruntest.WriteFile(t, modules, "config", "[remote \"origin\"]\n\turl = https://github.com/acme/submodule-excluded.git\n")
	hookruntest.WriteFile(t, sub, ".git", "gitdir: ../.git/modules/sub\n")
	hookruntest.WriteFile(t, sub, "file.txt", "made-up file\n")
	for _, tc := range []struct {
		name, cwd, path string
		want            antigravityOwner
	}{
		{"a path from the home folder", g.collected, "~/file.txt", ownerUnresolved},
		{"the home folder", g.collected, "~", ownerUnresolved},
		{"a dangling link", "", dangling, ownerUnresolved},
		{"a file through a dangling link", "", filepath.Join(dangling, "file.txt"), ownerUnresolved},
		{"a linked worktree", "", filepath.Join(linked, "new.txt"), ownerCollected},
		{"a submodule", "", filepath.Join(sub, "file.txt"), ownerExcluded},
	} {
		if got := antigravityPathOwner(g.pol, tc.cwd, tc.path); got != tc.want {
			t.Errorf("%s: owner %d, want %d", tc.name, got, tc.want)
		}
	}

	t.Setenv("GIT_DIR", filepath.Join(g.collected, ".git"))
	if got := antigravityPathOwner(g.pol, "", filepath.Join(g.excluded, "file.txt")); got != ownerUnresolved {
		t.Errorf("with GIT_DIR set: owner %d", got)
	}
	for _, pol := range []config.Policy{g.pol, {Mode: config.ModeGlobal}} {
		if antigravityWorkspacesCollected(hookrun.Env{Policy: pol, Cwd: g.collected}, &antigravityHookInput{}) {
			t.Errorf("with GIT_DIR set, mode %q collected the workspace", pol.Mode)
		}
	}
}

// The transcript's reader refuses a call whose arguments it cannot judge: none at all, or not
// a JSON object. {} and null name no file.
func TestAntigravityCallsRefusedWithArgumentsItCannotJudge(t *testing.T) {
	never := func(json.RawMessage) bool { return false }
	for _, tc := range []struct {
		name, call string
		want       bool
	}{
		{"no args", `{"name":"read_file"}`, true},
		{"a string", `{"name":"read_file","args":"made-up"}`, true},
		{"a number", `{"name":"read_file","args":1}`, true},
		{"an array", `{"name":"read_file","args":["made-up"]}`, true},
		{"an empty object", `{"name":"read_file","args":{}}`, false},
		{"null", `{"name":"read_file","args":null}`, false},
		{"an object", `{"name":"run_command","args":{"CommandLine":"made-up"}}`, false},
	} {
		var calls []antigravityToolCall
		if err := json.Unmarshal([]byte(`[{"name":"run_command","args":{}},`+tc.call+`]`), &calls); err != nil {
			t.Fatal(err)
		}
		if got := antigravityCallsRefused(calls, never); got != tc.want {
			t.Errorf("%s: refused %v, want %v", tc.name, got, tc.want)
		}
	}
}
