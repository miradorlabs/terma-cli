// Package reach reads what a tool call reached from its input, for hookrun.Env.Classify:
// the paths it names, and those a shell command in it reaches.
package reach

import (
	"encoding/json"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Spec is where a tool's input names what the call reached.
type Spec struct {
	// Paths are the keys that hold a path, or an array of paths.
	Paths []string
	// Commands are the keys that hold a shell command, and Dirs those that hold the folder
	// it runs in, relative to the hook's cwd.
	Commands, Dirs []string
}

// Of is what input reached, from cwd: each path it names under s's keys, the folder a
// shell command in it runs in, and what each such command reaches (hookrun.ShellPaths),
// UnknownReach included. It reads input only to classify: nothing of it is recorded.
func (s Spec) Of(input json.RawMessage, cwd string) []string {
	var args map[string]json.RawMessage
	if json.Unmarshal(input, &args) != nil {
		return nil
	}
	var out []string
	for _, k := range s.Paths {
		out = append(out, strings(args[k])...)
	}
	dir := cwd
	for _, k := range s.Dirs {
		if v := strings(args[k]); len(v) > 0 {
			dir = v[0]
			if !filepath.IsAbs(dir) && cwd != "" {
				dir = filepath.Join(cwd, dir)
			}
			out = append(out, dir)
			break
		}
	}
	for _, k := range s.Commands {
		for _, command := range strings(args[k]) {
			out = append(out, hookrun.ShellPaths(command, dir)...)
		}
	}
	return out
}

// Each agent's tools, by where their input names what a call reached.
var (
	// Copilot is the runtime's tools and VS Code's: read_file, list_dir, grep_search and
	// run_in_terminal.
	Copilot = Spec{
		Paths:    []string{"path", "file_path", "filePath", "notebook_path", "dirPath", "directory"},
		Commands: []string{"command"},
		Dirs:     []string{"cwd"},
	}
	// Cursor is Read's and Grep's paths, and Shell's command, run in its working directory.
	Cursor = Spec{
		Paths:    []string{"target_file", "file_path", "path", "target_directory", "directory", "filePath"},
		Commands: []string{"command"},
		Dirs:     []string{"cwd", "working_directory"},
	}
	// Gemini is read_file, read_many_files, list_directory, glob, search_file_content and
	// run_shell_command, whose directory is relative to the cwd.
	Gemini = Spec{
		Paths:    []string{"file_path", "absolute_path", "path", "dir_path", "paths"},
		Commands: []string{"command"},
		Dirs:     []string{"directory", "dir_path"},
	}
	// Antigravity is agy's own tools (view_file, list_dir, grep_search, find_by_name,
	// run_command in its Cwd) and the vendor tools it exposes per model family.
	Antigravity = Spec{
		Paths: []string{"AbsolutePath", "DirectoryPath", "SearchPath", "SearchDirectory", "Directory", "File", "TargetFile",
			"TargetDirectories", "target_file", "file_path", "path", "paths", "notebook_path", "dir_path"},
		Commands: []string{"CommandLine", "command"},
		Dirs:     []string{"Cwd", "cwd"},
	}
)

// strings is raw as a string or an array of strings; nil for anything else.
func strings(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	return nil
}
