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
	var globs []string
	for _, k := range s.Paths {
		out = append(out, readStrings(args[k])...)
	}
	for _, k := range s.Globs {
		globs = append(globs, readStrings(args[k])...)
	}
	if len(globs) > 0 && len(out) > 0 {
		out = append(out, hookrun.Search(out[0], globs...)[1:]...)
	} else if len(globs) > 0 && cwd != "" {
		out = hookrun.Search(cwd, globs...)
	}
	for _, k := range s.Lists {
		out = append(out, hookrun.Alone(readStrings(args[k])...)...)
	}
	dir := cwd
	for _, k := range s.Dirs {
		if v := readStrings(args[k]); len(v) > 0 {
			dir = v[0]
			if !filepath.IsAbs(dir) && cwd != "" {
				dir = filepath.Join(cwd, dir)
			}
			out = append(out, hookrun.Alone(dir)...)
			break
		}
	}
	for _, k := range s.Commands {
		for _, command := range readStrings(args[k]) {
			out = append(out, hookrun.Alone(hookrun.ShellPaths(command, dir)...)...)
		}
	}
	return out
}

// Per-agent input specs for tool-reach classification.
var (
	Copilot     = Spec{Paths: []string{"path", "file_path", "filePath", "notebook_path", "directory"}, Lists: []string{"dirPath"}, Globs: []string{"pattern", "glob", "includePattern"}, Commands: []string{"command"}, Dirs: []string{"cwd"}}
	Cursor      = Spec{Paths: []string{"target_file", "file_path", "path", "target_directory", "directory", "filePath"}, Globs: []string{"glob", "glob_pattern"}, Commands: []string{"command"}, Dirs: []string{"cwd", "working_directory"}}
	Gemini      = Spec{Paths: []string{"file_path", "absolute_path", "path", "dir_path", "paths"}, Globs: []string{"pattern", "include"}, Commands: []string{"command"}, Dirs: []string{"directory", "dir_path"}}
	Antigravity = Spec{
		Paths: []string{"AbsolutePath", "SearchPath", "SearchDirectory", "Directory", "File", "Path", "TargetFile", "TargetDirectories", "target_file", "file_path", "path", "paths", "notebook_path", "dir_path"},
		Lists: []string{"DirectoryPath"}, Globs: []string{"Pattern", "pattern", "glob"}, Commands: []string{"CommandLine", "command"}, Dirs: []string{"Cwd", "cwd"},
	}
)

func readStrings(raw json.RawMessage) (many []string) {
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}
	}
	_ = json.Unmarshal(raw, &many)
	return many
}
