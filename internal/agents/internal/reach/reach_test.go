package reach

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// call is a tool call's input and the paths Of reads from it.
type call struct {
	input string
	want  []string
}

// folder is where a tool call runs, holding NOTES.md and sub/NOTES.md, and sub.
func folder(t *testing.T) (cwd, sub string) {
	t.Helper()
	cwd, _ = filepath.EvalSymlinks(t.TempDir())
	sub = filepath.Join(cwd, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(cwd, "NOTES.md"), filepath.Join(sub, "NOTES.md")} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cwd, sub
}

// quoted is s as a JSON string.
func quoted(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func check(t *testing.T, spec Spec, cwd string, calls map[string]call) {
	t.Helper()
	for tool, c := range calls {
		if got := spec.Of(json.RawMessage(c.input), cwd); !slices.Equal(got, c.want) {
			t.Errorf("%s %s: Of = %q, want %q", tool, c.input, got, c.want)
		}
	}
}

// Copilot's tools name a path under one of several keys, a folder read below but list_dir's,
// and a glob a search matches; a search with a glob and no path searches the cwd.
// run_in_terminal names its command, run in the cwd unless the call names a folder, which it
// reaches too, alone, as each place the command reaches.
func TestCopilotInputs(t *testing.T) {
	t.Parallel()
	cwd, sub := folder(t)
	check(t, Copilot, cwd, map[string]call{
		"read_file":                 {`{"filePath": "/x/a.go", "startLine": 1, "endLine": 9}`, []string{"/x/a.go"}},
		"view":                      {`{"path": "sub/NOTES.md"}`, []string{"sub/NOTES.md"}},
		"list_dir":                  {`{"dirPath": "sub"}`, hookrun.Alone("sub")},
		"grep_search with no path":  {`{"query": "TODO", "includePattern": "*.go"}`, []string{cwd}},
		"run_in_terminal":           {`{"command": "cat NOTES.md", "explanation": "read", "isBackground": false}`, hookrun.Alone(cwd)},
		"run_in_terminal in sub":    {`{"command": "cat NOTES.md ../NOTES.md", "cwd": "sub"}`, hookrun.Alone(sub, sub, cwd)},
		"a notebook":                {`{"notebook_path": "a.ipynb"}`, []string{"a.ipynb"}},
		"a search in a directory":   {`{"pattern": "TODO", "directory": "sub"}`, []string{"sub"}},
		"a glob above a folder":     {`{"pattern": "../**/*.go", "path": "sub"}`, []string{"sub", "."}},
		"a shell command not known": {`{"command": "ls"}`, nil},
	})
}

// Cursor's Read and Grep name a file or a folder; Shell names its command and the folder
// it runs in, relative to the cwd or absolute.
func TestCursorInputs(t *testing.T) {
	t.Parallel()
	cwd, sub := folder(t)
	check(t, Cursor, cwd, map[string]call{
		"Read":                  {`{"target_file": "sub/NOTES.md", "offset": 1}`, []string{"sub/NOTES.md"}},
		"Read, by file_path":    {`{"file_path": "/x/a.go"}`, []string{"/x/a.go"}},
		"Grep":                  {`{"pattern": "TODO", "path": "sub", "glob": "*.go"}`, []string{"sub"}},
		"list_dir":              {`{"target_directory": "sub"}`, []string{"sub"}},
		"Shell":                 {`{"command": "cat NOTES.md"}`, hookrun.Alone(cwd)},
		"Shell in a folder":     {`{"command": "cat ../NOTES.md", "working_directory": "sub"}`, hookrun.Alone(sub, cwd)},
		"Shell in an absolute":  {`{"command": "cat NOTES.md", "cwd": ` + quoted(t, sub) + `}`, hookrun.Alone(sub, sub)},
		"a search with no path": {`{"query": "how are runs routed"}`, nil},
	})
}

// Gemini's read_many_files names an array of paths, list_directory a folder that is also
// where it runs, and run_shell_command its command and a directory relative to the cwd; a
// search with no path searches the cwd.
func TestGeminiInputs(t *testing.T) {
	t.Parallel()
	cwd, sub := folder(t)
	check(t, Gemini, cwd, map[string]call{
		"read_file":                     {`{"absolute_path": "/x/a.go"}`, []string{"/x/a.go"}},
		"read_file, by file_path":       {`{"file_path": "/x/a.go"}`, []string{"/x/a.go"}},
		"read_many_files":               {`{"paths": ["NOTES.md", "sub/NOTES.md"], "include": ["*.md"]}`, []string{"NOTES.md", "sub/NOTES.md"}},
		"list_directory":                {`{"dir_path": "sub"}`, append([]string{"sub"}, hookrun.Alone(sub)...)},
		"glob":                          {`{"pattern": "*.go", "path": "sub"}`, []string{"sub"}},
		"search_file_content, no path":  {`{"pattern": "TODO"}`, []string{cwd}},
		"run_shell_command":             {`{"command": "cat NOTES.md", "description": "read"}`, hookrun.Alone(cwd)},
		"run_shell_command in a folder": {`{"command": "cat NOTES.md", "directory": "sub"}`, hookrun.Alone(sub, sub)},
	})
}

// Antigravity's own tools name paths in CamelCase, TargetDirectories an array of them, and
// run_command its CommandLine and Cwd; the vendor tools it exposes keep their own keys.
func TestAntigravityInputs(t *testing.T) {
	t.Parallel()
	cwd, sub := folder(t)
	check(t, Antigravity, cwd, map[string]call{
		"view_file":           {`{"AbsolutePath": "/x/a.go", "StartLine": 1}`, []string{"/x/a.go"}},
		"list_dir":            {`{"DirectoryPath": "/x"}`, hookrun.Alone("/x")},
		"grep_search":         {`{"SearchPath": "/x", "Query": "TODO", "IsRegex": false}`, []string{"/x"}},
		"find_by_name":        {`{"SearchDirectory": "/x", "Pattern": "*.go"}`, []string{"/x"}},
		"codebase_search":     {`{"Query": "routing", "TargetDirectories": ["/x", "/y"]}`, []string{"/x", "/y"}},
		"run_command":         {`{"CommandLine": "cat NOTES.md", "Cwd": "sub", "Blocking": true}`, hookrun.Alone(sub, sub)},
		"a vendor read":       {`{"file_path": "sub/NOTES.md"}`, []string{"sub/NOTES.md"}},
		"a vendor shell call": {`{"command": "cat NOTES.md", "cwd": "sub"}`, hookrun.Alone(sub, sub)},
	})
}

// Of reads only an object, and only a string or an array of strings under its keys; a
// shell command whose reach is not known ends in UnknownReach, naming the command.
func TestOfReadsOnlyWhatItKnows(t *testing.T) {
	t.Parallel()
	cwd, _ := folder(t)
	check(t, Copilot, cwd, map[string]call{
		"not an object":    {`["a.go"]`, nil},
		"not JSON":         {`{"path": `, nil},
		"a number":         {`{"path": 7}`, nil},
		"an empty string":  {`{"path": ""}`, nil},
		"a mixed array":    {`{"path": ["a.go", 7]}`, nil},
		"no folder to run": {`{"command": "ls", "cwd": ""}`, nil},
	})
	got := Copilot.Of(json.RawMessage(`{"command": "cat $(cat p)/x"}`), cwd)
	if n := len(hookrun.UnknownReach); len(got) == 0 || len(got[len(got)-1]) < n || got[len(got)-1][:n] != hookrun.UnknownReach {
		t.Errorf("a command not resolved: Of = %q, want UnknownReach last", got)
	}
}

// crosses reports whether a call with input, made in side, a personal checkout beside Acme's,
// turns the run Mixed once Classify reads what spec says it reached.
func crosses(t *testing.T, spec Spec, input string) bool {
	t.Helper()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	side := hookruntest.RepoIn(t, parent, "github.com/dawson/side")
	hookruntest.RepoIn(t, parent, "github.com/acme/acme-api")
	st := hookruntest.RoutingState(t, []routing.Team{
		hookruntest.RoutedTeam("acme", "acme-platform", true, "github.com/acme/*"),
		hookruntest.RoutedPersonal("github.com/dawson/*"),
	}, map[string]string{"github.com/acme": "acme"})
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	env := hookrun.Env{StateDir: stateDir, ConfigDir: t.TempDir(), Now: time.Now(), Cwd: side, Spool: sp,
		Policy: config.NoPolicy("", ""), Routing: hookrun.NewRouting(hookruntest.RoutedProfile, st)}
	const sid = "search-session"
	env.StartRun(sid)
	env.Classify(context.Background(), nil, session.Session{ID: sid, Tool: "agent"}, spec.Of(json.RawMessage(input), side), nil)
	_, mixed := runs.MixedAt(stateDir, sid, time.Time{})
	return mixed
}

// Each agent's search of the folder above its checkout, by a folder its input names or a
// glob that climbs to it, reads Acme's checkout beside it, and turns the run Mixed; a search
// of its own checkout does not, nor does a listing of the folder above, which its key says it
// lists alone.
func TestEachAgentsSearchAboveTheCheckoutsCrosses(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		spec  Spec
		input string
		mixed bool
	}{
		"Copilot's grep":               {Copilot, `{"pattern": "TODO", "path": ".."}`, true},
		"Copilot's glob":               {Copilot, `{"pattern": "../**/*.go"}`, true},
		"Copilot's list_dir":           {Copilot, `{"dirPath": ".."}`, false},
		"Copilot's grep of its own":    {Copilot, `{"pattern": "TODO", "path": "."}`, false},
		"Cursor's Grep":                {Cursor, `{"pattern": "TODO", "path": ".."}`, true},
		"Cursor's Grep, a glob above":  {Cursor, `{"pattern": "TODO", "glob": "../**/*.md"}`, true},
		"Gemini's search_file_content": {Gemini, `{"pattern": "TODO", "dir_path": ".."}`, true},
		"Gemini's glob":                {Gemini, `{"pattern": "../**/*.go"}`, true},
		"Gemini's glob of its own":     {Gemini, `{"pattern": "**/*.go"}`, false},
		"Antigravity's grep_search":    {Antigravity, `{"SearchPath": "..", "Query": "TODO"}`, true},
		"Antigravity's find_by_name":   {Antigravity, `{"SearchDirectory": "..", "Pattern": "*.go"}`, true},
		"Antigravity's list_dir":       {Antigravity, `{"DirectoryPath": ".."}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := crosses(t, tc.spec, tc.input); got != tc.mixed {
				t.Errorf("Mixed %v, want %v", got, tc.mixed)
			}
		})
	}
}
