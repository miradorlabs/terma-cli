package reach

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
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

// Copilot's tools name a path under one of several keys; run_in_terminal names its command,
// run in the cwd unless the call names a folder, which it reaches too.
func TestCopilotInputs(t *testing.T) {
	t.Parallel()
	cwd, sub := folder(t)
	check(t, Copilot, cwd, map[string]call{
		"read_file":                 {`{"filePath": "/x/a.go", "startLine": 1, "endLine": 9}`, []string{"/x/a.go"}},
		"view":                      {`{"path": "sub/NOTES.md"}`, []string{"sub/NOTES.md"}},
		"list_dir":                  {`{"dirPath": "sub"}`, []string{"sub"}},
		"grep_search with no path":  {`{"query": "TODO", "includePattern": "*.go"}`, nil},
		"run_in_terminal":           {`{"command": "cat NOTES.md", "explanation": "read", "isBackground": false}`, []string{cwd}},
		"run_in_terminal in sub":    {`{"command": "cat NOTES.md ../NOTES.md", "cwd": "sub"}`, []string{sub, sub, cwd}},
		"a notebook":                {`{"notebook_path": "a.ipynb"}`, []string{"a.ipynb"}},
		"a search in a directory":   {`{"pattern": "TODO", "directory": "sub"}`, []string{"sub"}},
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
		"Shell":                 {`{"command": "cat NOTES.md"}`, []string{cwd}},
		"Shell in a folder":     {`{"command": "cat ../NOTES.md", "working_directory": "sub"}`, []string{sub, cwd}},
		"Shell in an absolute":  {`{"command": "cat NOTES.md", "cwd": ` + quoted(t, sub) + `}`, []string{sub, sub}},
		"a search with no path": {`{"query": "how are runs routed"}`, nil},
	})
}

// Gemini's read_many_files names an array of paths, list_directory a folder that is also
// where it runs, and run_shell_command its command and a directory relative to the cwd.
func TestGeminiInputs(t *testing.T) {
	t.Parallel()
	cwd, sub := folder(t)
	check(t, Gemini, cwd, map[string]call{
		"read_file":                     {`{"absolute_path": "/x/a.go"}`, []string{"/x/a.go"}},
		"read_file, by file_path":       {`{"file_path": "/x/a.go"}`, []string{"/x/a.go"}},
		"read_many_files":               {`{"paths": ["NOTES.md", "sub/NOTES.md"], "include": ["*.md"]}`, []string{"NOTES.md", "sub/NOTES.md"}},
		"list_directory":                {`{"dir_path": "sub"}`, []string{"sub", sub}},
		"glob":                          {`{"pattern": "*.go", "path": "sub"}`, []string{"sub"}},
		"search_file_content, no path":  {`{"pattern": "TODO"}`, nil},
		"run_shell_command":             {`{"command": "cat NOTES.md", "description": "read"}`, []string{cwd}},
		"run_shell_command in a folder": {`{"command": "cat NOTES.md", "directory": "sub"}`, []string{sub, sub}},
	})
}

// Antigravity's own tools name paths in CamelCase, TargetDirectories an array of them, and
// run_command its CommandLine and Cwd; the vendor tools it exposes keep their own keys.
func TestAntigravityInputs(t *testing.T) {
	t.Parallel()
	cwd, sub := folder(t)
	check(t, Antigravity, cwd, map[string]call{
		"view_file":           {`{"AbsolutePath": "/x/a.go", "StartLine": 1}`, []string{"/x/a.go"}},
		"list_dir":            {`{"DirectoryPath": "/x"}`, []string{"/x"}},
		"grep_search":         {`{"SearchPath": "/x", "Query": "TODO", "IsRegex": false}`, []string{"/x"}},
		"find_by_name":        {`{"SearchDirectory": "/x", "Pattern": "*.go"}`, []string{"/x"}},
		"codebase_search":     {`{"Query": "routing", "TargetDirectories": ["/x", "/y"]}`, []string{"/x", "/y"}},
		"run_command":         {`{"CommandLine": "cat NOTES.md", "Cwd": "sub", "Blocking": true}`, []string{sub, sub}},
		"a vendor read":       {`{"file_path": "sub/NOTES.md"}`, []string{"sub/NOTES.md"}},
		"a vendor shell call": {`{"command": "cat NOTES.md", "cwd": "sub"}`, []string{sub, sub}},
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
