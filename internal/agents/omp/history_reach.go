package omp

import (
	"cmp"
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents/internal/applypatch"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

var (
	ompHashlineHdrRe = regexp.MustCompile(`(?m)^\[([^\[\]\n]+)#[0-9A-Fa-f]{4}\]$`)
	ompHashlineTagRe = regexp.MustCompile(`#[0-9A-Fa-f]{4}$`)
)

type ompToolInput struct {
	Path    string   `json:"path,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	Move    string   `json:"move,omitempty"`
	Glob    string   `json:"glob,omitempty"`
	Include string   `json:"include,omitempty"`
	Pattern string   `json:"pattern,omitempty"`
	Command string   `json:"command,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Input   string   `json:"input,omitempty"`
	Diff    string   `json:"diff,omitempty"`
}

type ompToolDetails struct {
	ExitCode               *int64    `json:"exitCode,omitempty"`
	Cwd                    string    `json:"cwd,omitempty"`
	Path                   string    `json:"path,omitempty"`
	Move                   string    `json:"move,omitempty"`
	SourcePath             string    `json:"sourcePath,omitempty"`
	ResolvedPath           string    `json:"resolvedPath,omitempty"`
	DisplayTarget          string    `json:"displayTarget,omitempty"`
	MediaPath              string    `json:"mediaPath,omitempty"`
	DisplayReadTargets     []string  `json:"displayReadTargets,omitempty"`
	DisplayReadTargetLinks []*string `json:"displayReadTargetLinks,omitempty"`
	TouchesNoFiles         bool      `json:"touchesNoFiles,omitempty"`
	PerFileResults         []struct {
		Path       string `json:"path,omitempty"`
		Move       string `json:"move,omitempty"`
		SourcePath string `json:"sourcePath,omitempty"`
		IsError    bool   `json:"isError,omitempty"`
	} `json:"perFileResults,omitempty"`
	Meta json.RawMessage `json:"meta,omitempty"`
}

// ompToolCallReachPaths extracts filesystem paths reached by a tool call's arguments
// without consulting the uploader's live shell environment, matching terma-omp.ts.
func ompToolCallReachPaths(toolName string, rawArgs json.RawMessage, cwd string) []string {
	if len(rawArgs) == 0 {
		return nil
	}
	var in ompToolInput
	if json.Unmarshal(rawArgs, &in) != nil {
		return nil
	}
	var out []string
	addAlone := func(p string) {
		p, unknown := resolveReachPath(p)
		if unknown {
			out = append(out, hookrun.UnknownReach+"\x00"+p)
			return
		}
		if p != "" {
			out = append(out, hookrun.Alone(peelSelectorIfMissing(p, cwd))...)
		}
	}
	searchDirs := func() []string {
		var dirs []string
		addDir := func(p string) {
			p, unknown := resolveReachPath(p)
			if unknown {
				out = append(out, hookrun.UnknownReach+"\x00"+p)
				return
			}
			if p != "" {
				p = peelSelectorIfMissing(p, cwd)
				if !slices.Contains(dirs, p) {
					dirs = append(dirs, p)
				}
			}
		}
		addSplit := func(p string) {
			for _, seg := range splitOmpDelimitedPaths(p, cwd) {
				addDir(seg)
			}
		}
		if in.Path != "" {
			addSplit(in.Path)
		}
		for _, p := range in.Paths {
			addSplit(p)
		}
		if len(dirs) == 0 && cwd != "" {
			dirs = append(dirs, cwd)
		}
		return dirs
	}
	addDelimited := func(p string) {
		for _, seg := range splitOmpDelimitedPaths(p, cwd) {
			addAlone(seg)
		}
	}
	switch toolName {
	case "read":
		addDelimited(in.Path)
		for _, p := range in.Paths {
			addDelimited(p)
		}
	case "grep", "ast_grep":
		glob := cmp.Or(in.Glob, in.Include)
		for _, d := range searchDirs() {
			out = append(out, hookrun.SearchHistorical(d, glob)...)
		}
	case "find", "glob":
		glob := cmp.Or(in.Pattern, in.Glob)
		for _, d := range searchDirs() {
			out = append(out, hookrun.SearchHistorical(d, glob)...)
		}
	case "ls":
		addAlone(in.Path)
	case "bash":
		dir := cwd
		if raw := strings.TrimSpace(in.Cwd); raw != "" {
			resolved, unknown := resolveReachPath(raw)
			if unknown {
				out = append(out, hookrun.UnknownReach+"\x00"+raw)
			} else if resolved != "" {
				if !isAbsToolPath(resolved) && cwd != "" {
					dir = filepath.Join(cwd, resolved)
				} else {
					dir = resolved
				}
				out = append(out, hookrun.Alone(dir)...)
			}
		}
		if in.Command != "" {
			out = append(out, hookrun.Alone(hookrun.ShellPathsHistorical(in.Command, dir)...)...)
		}
	case "write":
		addAlone(unwrapHashlinePath(in.Path))
	case "edit", "apply_patch":
		addAlone(in.Path)
		addAlone(in.Move)
		for _, p := range in.Paths {
			addAlone(p)
		}
		for _, patch := range []string{in.Input, in.Diff} {
			if patch == "" {
				continue
			}
			for _, m := range ompHashlineHdrRe.FindAllStringSubmatch(patch, -1) {
				addAlone(m[1])
			}
			for _, p := range applypatch.Paths(patch) {
				addAlone(p)
			}
		}
	}
	return out
}

// ompToolResultReachPaths extracts any resolved paths disclosed in a tool result's details.
func ompToolResultReachPaths(toolName string, d *ompToolDetails, cwd string) []string {
	if d == nil {
		return nil
	}
	var out []string
	add := func(p string) {
		resolved, unknown := resolveReachPath(p)
		if unknown {
			out = append(out, hookrun.UnknownReach+"\x00"+p)
		} else if resolved != "" {
			out = append(out, hookrun.Alone(peelSelectorIfMissing(resolved, cwd))...)
		}
	}
	switch toolName {
	case "read":
		add(d.ResolvedPath)
		add(d.DisplayTarget)
		add(d.MediaPath)
		for _, target := range d.DisplayReadTargets {
			add(target)
		}
		for _, link := range d.DisplayReadTargetLinks {
			if link != nil {
				add(*link)
			}
		}
		if len(d.Meta) > 0 {
			var meta struct {
				Source struct {
					Type  string `json:"type"`
					Value string `json:"value"`
				} `json:"source"`
			}
			if json.Unmarshal(d.Meta, &meta) == nil && meta.Source.Type == "path" {
				add(meta.Source.Value)
			}
		}
	case "write":
		if !d.TouchesNoFiles {
			add(d.ResolvedPath)
		}
	case "edit", "apply_patch":
		add(d.Path)
		add(d.Move)
		add(d.SourcePath)
		for _, r := range d.PerFileResults {
			add(r.Path)
			add(r.Move)
			add(r.SourcePath)
		}
	case "bash":
		add(d.Cwd)
	}
	return out
}

// ompEditedFiles returns the confirmed edited file paths of a successful write/edit/apply_patch
// tool result, matching terma-omp.ts's editedFiles.
func ompEditedFiles(toolName string, rawArgs json.RawMessage, d *ompToolDetails, isError bool) []string {
	if isError {
		return nil
	}
	var files []string
	add := func(p string) {
		if p, unknown := resolveReachPath(p); !unknown && p != "" && !slices.Contains(files, p) {
			files = append(files, p)
		}
	}
	switch toolName {
	case "write":
		if d != nil && d.TouchesNoFiles {
			return nil
		}
		if d != nil && d.ResolvedPath != "" {
			add(d.ResolvedPath)
			return files
		}
		var in ompToolInput
		if json.Unmarshal(rawArgs, &in) == nil {
			add(nonURLPath(unwrapHashlinePath(in.Path)))
		}
	case "edit", "apply_patch":
		if d == nil {
			return nil
		}
		add(d.Path)
		add(d.Move)
		add(d.SourcePath)
		for _, r := range d.PerFileResults {
			if !r.IsError {
				add(r.Path)
				add(r.Move)
				add(r.SourcePath)
			}
		}
	}
	return files
}
