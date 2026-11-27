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
)

var (
	ompHashlineHdrRe = regexp.MustCompile(`(?m)^\[([^\[\]\n]+)#[0-9A-Fa-f]{4}\]$`)
	ompHashlineTagRe = regexp.MustCompile(`#[0-9A-Fa-f]{4}$`)
)

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
	Path               string          `json:"path,omitempty"`
	Paths              []string        `json:"paths,omitempty"`
	Move               string          `json:"move,omitempty"`
	Glob               string          `json:"glob,omitempty"`
	Include            string          `json:"include,omitempty"`
	Pattern            string          `json:"pattern,omitempty"`
	Command            string          `json:"command,omitempty"`
	Cwd                string          `json:"cwd,omitempty"`
	Input              json.RawMessage `json:"input,omitempty"`
	Diff               string          `json:"diff,omitempty"`
	Action             string          `json:"action,omitempty"`
	File               string          `json:"file,omitempty"`
	NewName            string          `json:"new_name,omitempty"`
	OutputPath         string          `json:"output_path,omitempty"`
	IncludePaths       []string        `json:"include_paths,omitempty"`
	KnowledgeBasePaths []string        `json:"knowledge_base_paths,omitempty"`
}

type ompToolDetails struct {
	ExitCode               *int64    `json:"exitCode,omitempty"`
	Cwd                    string    `json:"cwd,omitempty"`
	Path                   string    `json:"path,omitempty"`
	Move                   string    `json:"move,omitempty"`
	SourcePath             string    `json:"sourcePath,omitempty"`
	ResolvedPath           string    `json:"resolvedPath,omitempty"`
	ScopePath              string    `json:"scopePath,omitempty"`
	DisplayTarget          string    `json:"displayTarget,omitempty"`
	MediaPath              string    `json:"mediaPath,omitempty"`
	ImagePaths             []string  `json:"imagePaths,omitempty"`
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
	switch toolName {
	case "eval", "debug", "browser", "computer", "run_code", "python":
		return []string{hookrun.UnknownReach + "\x00" + toolName}
	}
	if len(rawArgs) == 0 {
		return nil
	}
	var in ompToolInput
	if json.Unmarshal(rawArgs, &in) != nil {
		switch toolName {
		case "read", "grep", "search", "ast_grep", "ast_edit", "find", "glob", "ls", "lsp", "security_scan", "generate_image", "tts", "bash", "write", "edit", "apply_patch":
			return []string{hookrun.UnknownReach + "\x00" + toolName}
		}
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
			// Leading '!' entries are OMP exclusion filters (e.g. "!vendor"), not search roots;
			// skip them unless a literal entry of that name exists on disk so cwd is still scanned.
			if strings.HasPrefix(p, "!") && !statReachExists(p, cwd) {
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
	searchWithGlobs := func(rawGlobs ...string) {
		var globs []string
		for _, rawGlob := range rawGlobs {
			for _, part := range splitTopLevel(strings.TrimSpace(normalizeUnicodeSpaces(rawGlob)), false, false) {
				resolved, unknown := resolveReachPath(part)
				if unknown {
					out = append(out, hookrun.UnknownReach+"\x00"+part)
					continue
				}
				if resolved != "" {
					globs = append(globs, strings.ReplaceAll(resolved, `\`, "/"))
				}
			}
		}
		for _, d := range searchDirs() {
			baseDir, embeddedGlob, unknown := parseOmpSearchPath(d, cwd)
			if unknown {
				out = append(out, hookrun.UnknownReach+"\x00"+d)
				continue
			}
			if embeddedGlob != "" {
				allGlobs := make([]string, 0, len(globs)+1)
				allGlobs = append(allGlobs, embeddedGlob)
				allGlobs = append(allGlobs, globs...)
				out = append(out, hookrun.SearchHistorical(baseDir, allGlobs...)...)
				continue
			}
			out = append(out, hookrun.SearchHistorical(baseDir, globs...)...)
		}
	}
	switch toolName {
	case "read":
		addDelimited(in.Path)
		for _, p := range in.Paths {
			addDelimited(p)
		}
	case "grep", "search", "ast_grep", "ast_edit":
		searchWithGlobs(in.Glob, in.Include)
	case "find", "glob":
		searchWithGlobs(in.Pattern, in.Glob)
	case "ls":
		addAlone(in.Path)
	case "lsp":
		addAlone(in.File)
		if in.Action == "rename_file" {
			addAlone(in.NewName)
		}
	case "security_scan":
		for _, p := range in.IncludePaths {
			addDelimited(p)
		}
		for _, p := range in.KnowledgeBasePaths {
			addDelimited(p)
		}
	case "generate_image":
		if len(in.Input) > 0 && string(in.Input) != "null" {
			var imgs []struct {
				Path string `json:"path,omitempty"`
			}
			if json.Unmarshal(in.Input, &imgs) != nil {
				out = append(out, hookrun.UnknownReach+"\x00generate_image")
			} else {
				for _, img := range imgs {
					addAlone(img.Path)
				}
			}
		}
	case "tts":
		addAlone(in.OutputPath)
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
		addAlone(unwrapHashlinePath(in.Path))
		addAlone(unwrapHashlinePath(in.Move))
		for _, p := range in.Paths {
			addAlone(p)
		}
		var inputStr string
		if len(in.Input) > 0 {
			_ = json.Unmarshal(in.Input, &inputStr)
		}
		for _, patch := range []string{inputStr, in.Diff} {
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

// parseOmpSearchPath mirrors OMP's parseSearchPathPreferringLiteral: it preserves existing
// literal paths, otherwise normalizes backslashes to forward slashes (matching OMP's
// normalizePathSeparators on every OS) and splits at the first glob segment (*, ?, [, {) into
// (baseDir, embeddedGlob). Root-level globs (/*/*.txt, /**/*.txt, C:/*.txt) fail closed.
func parseOmpSearchPath(d, cwd string) (baseDir, embeddedGlob string, unknown bool) {
	if statReachExists(d, cwd) {
		return d, "", false
	}
	slashed := strings.ReplaceAll(d, `\`, "/")
	if !strings.ContainsAny(slashed, "*?[{") {
		return filepath.FromSlash(slashed), "", false
	}
	segs := strings.Split(slashed, "/")
	firstGlob := slices.IndexFunc(segs, func(s string) bool { return strings.ContainsAny(s, "*?[{") })
	if firstGlob < 0 {
		return filepath.FromSlash(slashed), "", false
	}
	embeddedGlob = strings.Join(segs[firstGlob:], "/")
	switch {
	case firstGlob == 0:
		baseDir = cmp.Or(cwd, ".")
	case firstGlob == 1 && segs[0] == "":
		return d, "", true
	case firstGlob == 1 && len(segs[0]) == 2 && isASCIILetter(segs[0][0]) && segs[0][1] == ':':
		return d, "", true
	default:
		baseDir = filepath.FromSlash(strings.Join(segs[:firstGlob], "/"))
		if !isAbsToolPath(baseDir) && cwd != "" {
			baseDir = filepath.Join(cwd, baseDir)
		}
		if isRootSearchDir(baseDir) {
			return d, "", true
		}
	}
	return baseDir, embeddedGlob, false
}

func isRootSearchDir(p string) bool {
	c := filepath.ToSlash(filepath.Clean(p))
	return c == "/" || (len(c) == 3 && isASCIILetter(c[0]) && c[1] == ':' && c[2] == '/')
}

func translateWindowsDriveOnLinux(p string) (string, bool) {
	if strings.HasPrefix(p, `\\`) {
		return p, true
	}
	if !isWindowsDrivePath(p) {
		return p, false
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != "" {
		drive := strings.ToLower(p[:1])
		mount := "/mnt/" + drive
		if fi, err := os.Stat(mount); err == nil && fi.IsDir() {
			rest := strings.TrimLeft(strings.ReplaceAll(p[2:], `\`, "/"), "/")
			if rest == "" {
				return mount, false
			}
			return mount + "/" + rest, false
		}
	}
	return p, true
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
	case "ast_edit", "ast_grep":
		add(d.ScopePath)
		add(d.Path)
	case "generate_image":
		for _, p := range d.ImagePaths {
			add(p)
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
			add(unwrapHashlinePath(in.Path))
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
