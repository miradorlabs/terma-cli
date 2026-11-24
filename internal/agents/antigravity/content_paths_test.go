package antigravity

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
)

// pathRig is a collected checkout, a second collected one, an excluded one, an excluded clone
// nested in the collected one, a folder of the collected one that does not hold the clone,
// and a folder in no repository.
type pathRig struct {
	collected, other, excluded, nested, sub, none string
	pol                                           config.Policy
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
	g.sub = filepath.Join(g.collected, "sub")
	for _, dir := range []string{g.collected, g.other, g.excluded, g.nested, g.sub, g.none} {
		hookruntest.WriteFile(t, dir, "file.txt", "made-up file\n")
	}
	g.pol = config.DefaultPolicy()
	g.pol.Repositories = append(hookruntest.Admitting(g.collected).Repositories, hookruntest.Admitting(g.other).Repositories...)
	return g
}

// Which checkout owns a path, judged by the policy: collected, excluded, in none, or not
// placed. Where gitx places a path is its own test (gitx.TestPlaceFS).
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
		{"relative to the cwd", g.collected, "file.txt", ownerCollected},
		{"relative, climbing into an excluded checkout", g.collected, filepath.Join("..", filepath.Base(g.excluded), "file.txt"), ownerExcluded},
		{"relative with no cwd", "", "file.txt", ownerUnresolved},
		{"a missing path that climbs from the cwd", g.collected, "missing" + string(filepath.Separator) + ".." + string(filepath.Separator) + "file.txt", ownerUnresolved},
	} {
		if got := newAntigravityPaths(g.pol).owner(tc.cwd, tc.path); got != tc.want {
			t.Errorf("%s: owner %d, want %d", tc.name, got, tc.want)
		}
	}
	// Global mode collects every checkout.
	if got := newAntigravityPaths(config.Policy{Mode: config.ModeGlobal}).owner("", filepath.Join(g.excluded, "file.txt")); got != ownerCollected {
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
		return newAntigravityPaths(g.pol).refuses(g.collected, in.ToolCall.Args)
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
		{"run_command", "Cwd", g.sub, false},
		// The checkout's root holds the excluded clone nested in it.
		{"run_command", "Cwd", g.collected, true},
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
	if newAntigravityPaths(config.Policy{Mode: config.ModeGlobal}).refuses(g.collected, global.ToolCall.Args) {
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

// A call whose arguments name more paths than terma judges is refused, though every one is
// collected: the count runs across the call's keys, and a value that is not a path counts
// for nothing.
func TestAntigravityRefusesACallNamingTooManyPaths(t *testing.T) {
	g := newPathRig(t)
	file := filepath.Join(g.collected, "file.txt")
	for _, n := range []int{antigravityPathsPerCall, antigravityPathsPerCall + 1} {
		list := make([]string, n-1)
		for i := range list {
			list[i] = file
		}
		args, err := json.Marshal(map[string]any{"Paths": list, "Path": file, "Query": "made-up", "Limit": 3})
		if err != nil {
			t.Fatal(err)
		}
		want := n > antigravityPathsPerCall
		if got := newAntigravityPaths(g.pol).refuses(g.collected, args); got != want {
			t.Errorf("%d collected paths: refused %v, want %v", n, got, want)
		}
	}
}

// Content goes only when every workspace is in a checkout the policy collects: a second
// workspace in an excluded checkout, or outside git, stops it; global mode collects both.
func TestAntigravityWorkspacesMustAllBeCollected(t *testing.T) {
	g := newPathRig(t)
	collected := func(pol config.Policy, cwd string, workspaces ...string) bool {
		return newAntigravityPaths(pol).workspacesCollected(cwd, &antigravityHookInput{WorkspacePaths: workspaces})
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

// Where a path's checkout cannot be trusted, it is unresolved: a path that starts with "~",
// and every path with GIT_DIR in the hook's environment, so no workspace is collected then,
// global mode or not.
func TestAntigravityPathOwnerFailsClosed(t *testing.T) {
	g := newPathRig(t)
	for _, tc := range []struct {
		name, path string
	}{
		{"a path from the home folder", "~/file.txt"},
		{"the home folder", "~"},
		{"another user's home folder", "~someone/file.txt"},
	} {
		if got := newAntigravityPaths(g.pol).owner(g.collected, tc.path); got != ownerUnresolved {
			t.Errorf("%s: owner %d", tc.name, got)
		}
	}

	t.Setenv("GIT_DIR", filepath.Join(g.collected, ".git"))
	if got := newAntigravityPaths(g.pol).owner("", filepath.Join(g.excluded, "file.txt")); got != ownerUnresolved {
		t.Errorf("with GIT_DIR set: owner %d", got)
	}
	for _, pol := range []config.Policy{g.pol, {Mode: config.ModeGlobal}} {
		if newAntigravityPaths(pol).workspacesCollected(g.collected, &antigravityHookInput{}) {
			t.Errorf("with GIT_DIR set, mode %q collected the workspace", pol.Mode)
		}
	}
}

// The transcript's reader refuses a call whose arguments it cannot judge: none at all, or not
// a JSON object. {} and null name no file. With no rule to judge by, it refuses any call.
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
		if !antigravityCallsRefused(calls, nil) {
			t.Errorf("%s: shown with no rule to judge by", tc.name)
		}
	}
	if antigravityCallsRefused(nil, nil) {
		t.Error("no calls refused with no rule to judge by")
	}
}
