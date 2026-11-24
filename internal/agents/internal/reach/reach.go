// Package reach extracts paths and shell reaches from tool inputs for hookrun.Env.Classify.
package reach

import (
	"encoding/json"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Spec names the JSON keys where a tool's input specifies paths, listings, globs, or shell commands.
type Spec struct{ Paths, Lists, Globs, Commands, Dirs []string }

// Of extracts the paths and shell reaches from input relative to cwd for Classify.
func (s Spec) Of(input json.RawMessage, cwd string) (out []string) {
	var args map[string]json.RawMessage
	if json.Unmarshal(input, &args) != nil {
		return nil
	}
	vals := func(keys []string) (got []string) {
		for _, k := range keys {
			got = append(got, readStrings(args[k])...)
		}
		return got
	}
	out = vals(s.Paths)
	if globs := vals(s.Globs); len(globs) > 0 {
		if len(out) > 0 {
			out = append(out, hookrun.Search(out[0], globs...)[1:]...)
		} else if cwd != "" {
			out = hookrun.Search(cwd, globs...)
		}
	}
	out = append(out, hookrun.Alone(vals(s.Lists)...)...)
	dir := cwd
	if dirs := vals(s.Dirs); len(dirs) > 0 {
		if dir = dirs[0]; !filepath.IsAbs(dir) && cwd != "" {
			dir = filepath.Join(cwd, dir)
		}
		out = append(out, hookrun.Alone(dir)...)
	}
	for _, c := range vals(s.Commands) {
		out = append(out, hookrun.Alone(hookrun.ShellPaths(c, dir)...)...)
	}
	return out
}

// Per-agent input specs for tool-reach classification.
var (
	Copilot     = Spec{Paths: []string{"path", "file_path", "filePath", "notebook_path", "directory"}, Lists: []string{"dirPath"}, Globs: []string{"pattern", "glob", "includePattern"}, Commands: []string{"command"}, Dirs: []string{"cwd"}}
	Cursor      = Spec{Paths: []string{"target_file", "file_path", "path", "target_directory", "directory", "filePath"}, Globs: []string{"glob", "glob_pattern"}, Commands: []string{"command"}, Dirs: []string{"cwd", "working_directory"}}
	Gemini      = Spec{Paths: []string{"file_path", "absolute_path", "path", "dir_path", "paths"}, Globs: []string{"pattern", "include"}, Commands: []string{"command"}, Dirs: []string{"directory", "dir_path"}}
	Antigravity = Spec{Paths: []string{"AbsolutePath", "SearchPath", "SearchDirectory", "Directory", "File", "Path", "TargetFile", "TargetDirectories", "target_file", "file_path", "path", "paths", "notebook_path", "dir_path"}, Lists: []string{"DirectoryPath"}, Globs: []string{"Pattern", "pattern", "glob"}, Commands: []string{"CommandLine", "command"}, Dirs: []string{"Cwd", "cwd"}}
)

func readStrings(raw json.RawMessage) (many []string) {
	if one := ""; json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}
	}
	_ = json.Unmarshal(raw, &many)
	return many
}
