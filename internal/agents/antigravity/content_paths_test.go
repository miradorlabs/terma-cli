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
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
)

// pathRig is a collected checkout, a second collected one, an excluded one, an excluded clone
// nested in the collected one, a folder of the collected one, and a folder in no repository.
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

// A call of one of agy's own file and folder tools is judged by the paths under its keys:
// refused when one is in a checkout the policy does not collect, or cannot be placed. A file in
// no repository is shown; a folder there is refused outside global mode. Every tool and key of
// the table is checked, and arguments terma cannot judge are refused.
func TestAntigravityJudgesAgysOwnFileAndFolderTools(t *testing.T) {
	g := newPathRig(t)
	link := filepath.Join(g.collected, "into-excluded")
	links := os.Symlink(g.excluded, link) == nil
	global := config.Policy{Mode: config.ModeGlobal}
	type row struct {
		name, path string
		pol        config.Policy
		want       bool
	}
	for name, tool := range antigravityPathTools {
		// at is what the tool names in dir: the folder itself, or its file.
		at := func(dir string) string {
			if tool.folder {
				return dir
			}
			return filepath.Join(dir, "file.txt")
		}
		climb, err := filepath.Rel(g.collected, at(g.excluded))
		if err != nil {
			t.Fatal(err)
		}
		inside, err := filepath.Rel(g.collected, at(g.sub))
		if err != nil {
			t.Fatal(err)
		}
		rows := []row{
			{"the collected checkout", at(g.sub), g.pol, false},
			{"an excluded checkout", at(g.excluded), g.pol, true},
			{"an excluded clone nested in the collected one", at(g.nested), g.pol, true},
			{"relative inside the checkout", inside, g.pol, false},
			{"relative, climbing into an excluded checkout", climb, g.pol, true},
			{"the home folder", "~/x", g.pol, true},
			{"no repository", at(g.none), g.pol, tool.folder},
			{"no repository, global mode", at(g.none), global, false},
		}
		if links {
			rows = append(rows, row{"through a link into an excluded checkout", at(link), g.pol, true})
		}
		for _, key := range tool.keys {
			for _, r := range rows {
				args := argsJSON(t, map[string]any{key: r.path, "Other": 1})
				if got := newAntigravityPaths(r.pol).refuses(g.collected, name, args); got != r.want {
					t.Errorf("%s %s, %s: refused %v, want %v", name, key, r.name, got, r.want)
				}
			}
		}
		for _, raw := range []string{"", `"made-up"`, `1`, `["made-up"]`, `null`, `{}`, `{"` + tool.keys[0] + `":1}`} {
			if !newAntigravityPaths(g.pol).refuses(g.collected, name, json.RawMessage(raw)) {
				t.Errorf("%s with arguments %q: shown", name, raw)
			}
		}
	}

	defer func(was bool) { antigravityShowsFilesInNoRepository = was }(antigravityShowsFilesInNoRepository)
	antigravityShowsFilesInNoRepository = false
	file := func(path string) json.RawMessage { return argsJSON(t, map[string]any{"Path": path}) }
	if !newAntigravityPaths(g.pol).refuses(g.collected, "read_file", file(filepath.Join(g.none, "file.txt"))) {
		t.Error("a file in no repository was shown with the answer withholding it")
	}
	if newAntigravityPaths(g.pol).refuses(g.collected, "read_file", file(filepath.Join(g.collected, "file.txt"))) {
		t.Error("withholding files in no repository refused a collected one")
	}
}

// A decision, not a gap: terma judges no other tool's arguments, an MCP tool's, a shell
// command's or one it has never seen, whatever they name. Claude Code's and Codex's tools are
// not checked either; their content goes under the session's consent.
func TestAntigravityOtherToolsAreNotJudgedAsClaudeCodesAndCodexsAreNot(t *testing.T) {
	g := newPathRig(t)
	for _, c := range otherToolCalls(filepath.Join(g.excluded, "file.txt")) {
		var call antigravityToolCall
		if err := json.Unmarshal([]byte(c), &call); err != nil {
			t.Fatal(err)
		}
		if newAntigravityPaths(g.pol).refuses(g.collected, call.Name, call.Args) {
			t.Errorf("%s: refused", c)
		}
	}
}

// otherToolCalls are calls of tools outside antigravityPathTools whose arguments name path in
// ways a table tool's would be refused for; the last has no arguments.
func otherToolCalls(path string) []string {
	q := `"` + hookruntest.InJSON(path) + `"`
	return []string{
		`{"name":"mcp_server_read","args":{"path":` + q + `}}`,
		`{"name":"call_mcp_tool","args":{"ServerName":"made-up","ToolName":"read","Arguments":{"path":` + q + `}}}`,
		`{"name":"run_command","args":{"CommandLine":"cat ` + hookruntest.InJSON(path) + `"}}`,
		`{"name":"made_up_tool","args":{"uri":"file://` + hookruntest.InJSON(filepath.ToSlash(path)) + `","paths":[` + q + `,` + q + `]}}`,
		`{"name":"made_up_tool"}`,
	}
}

// argsJSON is v as a call's arguments.
func argsJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The reader judges a call line's calls by name and arguments: one refused call refuses the
// line, and a line of other tools is never refused, an argument-less one included. With no
// rule to judge by, any call is refused.
func TestAntigravityCallsRefused(t *testing.T) {
	g := newPathRig(t)
	refuses := func(name string, args json.RawMessage) bool {
		return newAntigravityPaths(g.pol).refuses(g.collected, name, args)
	}
	calls := func(raw ...string) []antigravityToolCall {
		var out []antigravityToolCall
		if err := json.Unmarshal([]byte("["+strings.Join(raw, ",")+"]"), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	others := otherToolCalls(filepath.Join(g.excluded, "file.txt"))
	for _, tc := range []struct {
		name  string
		calls []antigravityToolCall
		want  bool
	}{
		{"other tools only", calls(others...), false},
		{"a collected read among them", calls(append(others, readCall(filepath.Join(g.collected, "file.txt")))...), false},
		{"an excluded read among them", calls(append(others, readCall(filepath.Join(g.excluded, "file.txt")))...), true},
		{"a read with no arguments", calls(`{"name":"read_file"}`), true},
	} {
		if got := antigravityCallsRefused(tc.calls, refuses); got != tc.want {
			t.Errorf("%s: refused %v, want %v", tc.name, got, tc.want)
		}
		if !antigravityCallsRefused(tc.calls, nil) {
			t.Errorf("%s: shown with no rule to judge by", tc.name)
		}
	}
	if antigravityCallsRefused(nil, nil) {
		t.Error("no calls refused with no rule to judge by")
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
// global mode or not, and no call of a table tool is shown.
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
	if !newAntigravityPaths(g.pol).refuses(g.collected, "read_file", argsJSON(t, map[string]any{"Path": filepath.Join(g.collected, "file.txt")})) {
		t.Error("with GIT_DIR set, a read was shown")
	}
	for _, pol := range []config.Policy{g.pol, {Mode: config.ModeGlobal}} {
		if newAntigravityPaths(pol).workspacesCollected(g.collected, &antigravityHookInput{}) {
			t.Errorf("with GIT_DIR set, mode %q collected the workspace", pol.Mode)
		}
	}
}
