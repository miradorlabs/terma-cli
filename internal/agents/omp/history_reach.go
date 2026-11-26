package omp

import (
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/miradorlabs/terma-cli/internal/agents/internal/applypatch"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
)

var (
	ompInternalURLRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)
	ompHashlineHdrRe = regexp.MustCompile(`(?m)^\[([^\[\]\n]+)#[0-9A-Fa-f]{4}\]$`)
	ompHashlineTagRe = regexp.MustCompile(`#[0-9A-Fa-f]{4}$`)
)

func nonURLPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || ompInternalURLRe.MatchString(p) {
		return ""
	}
	return p
}

// unwrapHashlinePath strips an optional [PATH] or [PATH#ABCD] hashline header wrapper,
// matching adapter/omp/tool_trace.go's unwrapHashlinePath.
func unwrapHashlinePath(p string) string {
	inner, opened := strings.CutPrefix(strings.TrimRightFunc(p, unicode.IsSpace), "[")
	inner, closed := strings.CutSuffix(inner, "]")
	if !opened || !closed {
		return p
	}
	if loc := ompHashlineTagRe.FindStringIndex(inner); loc != nil {
		inner = inner[:loc[0]]
	} else if strings.Contains(inner, "#") {
		return p
	}
	return cmp.Or(inner, p)
}

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
	ExitCode           *int64   `json:"exitCode,omitempty"`
	Cwd                string   `json:"cwd,omitempty"`
	Path               string   `json:"path,omitempty"`
	Move               string   `json:"move,omitempty"`
	SourcePath         string   `json:"sourcePath,omitempty"`
	ResolvedPath       string   `json:"resolvedPath,omitempty"`
	DisplayReadTargets []string `json:"displayReadTargets,omitempty"`
	TouchesNoFiles     bool     `json:"touchesNoFiles,omitempty"`
	PerFileResults     []struct {
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
			out = append(out, hookrun.Alone(p)...)
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
			if p != "" && !slices.Contains(dirs, p) {
				dirs = append(dirs, p)
			}
		}
		addSplit := func(p string) {
			if strings.Contains(p, ";") {
				for seg := range strings.SplitSeq(p, ";") {
					addDir(seg)
				}
				return
			}
			addDir(p)
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
		if strings.Contains(p, ";") {
			for seg := range strings.SplitSeq(p, ";") {
				addAlone(seg)
			}
			return
		}
		addAlone(p)
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

// resolveReachPath filters internal URLs and expands a leading ~ / ~/ / ~\ to the user's
// home directory so reach classification never joins ~/... under the session cwd. An
// unexpandable ~ path (missing home dir or ~otheruser/...) returns unknown=true to fail closed.
func resolveReachPath(raw string) (string, bool) {
	p := nonURLPath(raw)
	if p == "" {
		return "", false
	}
	if !strings.HasPrefix(p, "~") {
		return p, false
	}
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p, true
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p, true
	}
	if p == "~" {
		return home, false
	}
	return filepath.Join(home, p[2:]), false
}

// ompParentSessionID extracts a parent session ID from header.parentSession or
// header.continuationOf (which may be a bare session ID or a transcript path).
func ompParentSessionID(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if session.ValidID(ref) {
		return ref
	}
	return ompSessionIDFromFilename(filepath.Base(strings.ReplaceAll(ref, `\`, "/")))
}

// sameParentSessionPath reports whether headerParent names parentPath (the parent transcript
// corresponding to subPath's containing directory), matching terma-omp.ts's parentOf rule
// while tolerating slash/case normalization on Windows and symlinked session roots.
func sameParentSessionPath(subPath, parentPath, headerParent string, windows bool) bool {
	if strings.TrimSpace(headerParent) == "" || parentPath == "" {
		return false
	}
	norm := func(p string) string {
		if windows {
			p = strings.ReplaceAll(p, `\`, "/")
		}
		p = filepath.ToSlash(filepath.Clean(p))
		if windows {
			p = strings.ToLower(p)
		}
		return p
	}
	expected := norm(parentPath)
	if norm(headerParent) == expected {
		return true
	}
	if !windows {
		subDirReal, err1 := filepath.EvalSymlinks(filepath.Dir(subPath))
		hdrDirReal, err2 := filepath.EvalSymlinks(filepath.Dir(headerParent))
		if err1 == nil && err2 == nil && norm(subDirReal+".jsonl") == norm(filepath.Join(hdrDirReal, filepath.Base(headerParent))) {
			return true
		}
	}
	return false
}

func isAbsToolPath(p string) bool {
	if filepath.IsAbs(p) {
		return true
	}
	return len(p) >= 3 && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

// ompToolResultReachPaths extracts any resolved paths disclosed in a tool result's details.
func ompToolResultReachPaths(toolName string, d *ompToolDetails) []string {
	if d == nil {
		return nil
	}
	var out []string
	add := func(p string) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, hookrun.Alone(p)...)
		}
	}
	switch toolName {
	case "read":
		add(d.ResolvedPath)
		for _, target := range d.DisplayReadTargets {
			add(nonURLPath(target))
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

func boundOmpToolArgs(raw json.RawMessage) string {
	compact := compactRawJSON(raw)
	if len(compact) <= ompMaxToolArgs {
		return compact
	}
	// Preserve small path/command metadata for target-resource extraction while capping huge
	// write/edit bodies so a single span stays well beneath the 3.5 MiB OTLP ceiling.
	var in ompToolInput
	if json.Unmarshal(raw, &in) == nil {
		proj := map[string]any{"_truncated": true}
		if in.Path != "" {
			proj["path"] = in.Path
		}
		if len(in.Paths) > 0 {
			proj["paths"] = in.Paths
		}
		if in.Move != "" {
			proj["move"] = in.Move
		}
		if in.Cwd != "" {
			proj["cwd"] = in.Cwd
		}
		if in.Command != "" {
			proj["command"] = cutUTF8(in.Command, 4096)
		}
		if s := marshalCompactJSON(proj); s != "" && len(s) <= ompMaxToolArgs {
			return s
		}
	}
	return `{"_truncated":true}`
}
