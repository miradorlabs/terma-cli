package omp

import (
	"cmp"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

var ompHashlineTagRe = regexp.MustCompile(`#[0-9A-Fa-f]{4}$`)

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
	Command string   `json:"command,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
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
	IsDirectory            bool      `json:"isDirectory,omitempty"`
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

// ompEditedFiles returns the confirmed edited file paths of a successful write/edit/apply_patch
// tool result, matching terma-omp.ts's editedFiles.
func ompEditedFiles(toolName string, rawArgs json.RawMessage, d *ompToolDetails, isError bool) []string {
	if isError {
		return nil
	}
	var files []string
	add := func(p string) {
		if p, unknown := resolveOmpPath(p); !unknown && p != "" && !slices.Contains(files, p) {
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
