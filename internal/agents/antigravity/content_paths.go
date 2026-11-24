package antigravity

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Which content a hook may attach, and the two rules for content of another repository
// (design 3.3). Both ask one question of a path: which checkout owns it.

// antigravityShowsFilesInNoRepository is the answer for a file in no repository at all (a
// skill file, a note in the home folder): shown, as Claude Code and Codex show such reads.
// false refuses it as a file in a repository the policy does not collect.
var antigravityShowsFilesInNoRepository = true

// antigravityWithholdsRepliesAfterRefusal withholds the replies of a turn after a tainted call
// line (transcript_read.go), until that turn's record ends: the model can repeat what it
// read. false sends them, as every other agent's are.
var antigravityWithholdsRepliesAfterRefusal = false

// antigravityContentKeys are the argument keys that name a file or a folder, for any tool:
// the edit tools' (antigravityPathKeys), view_file's, and the folder a tool lists, searches or
// runs in (list_dir, grep_search, find_by_name, and run_command where a version sends Cwd).
var antigravityContentKeys = append(slices.Clone(antigravityPathKeys), "AbsolutePath", "DirectoryPath", "SearchPath", "SearchDirectory", "Cwd")

// contentConsented reports whether the developer chose Antigravity, or global mode collects
// every session: what was said travels under it, the team's policy deciding which kinds.
func contentConsented(c hookrun.Consent) bool {
	return c.Global || slices.Contains(c.Agents, Agent{}.Name())
}

// antigravityContent is which kinds of content a hook may attach: each needs its policy
// switch, the developer's consent, and every workspace in a checkout the policy collects.
type antigravityContent struct{ prompts, tools bool }

// antigravityContentFor is the content the hook in e (its Cwd where it ran) may attach.
func antigravityContentFor(e hookrun.Env, in *antigravityHookInput) antigravityContent {
	prompts, tools := e.Policy.Content()
	if !prompts && !tools || !contentConsented(e.Consent()) || !antigravityWorkspacesCollected(e, in) {
		return antigravityContent{}
	}
	return antigravityContent{prompts: prompts, tools: tools}
}

// antigravityWorkspacesCollected reports whether every one of the conversation's workspaces
// (workspacePaths, the cwd when the list is empty) is in a checkout the policy collects,
// judged by the owner of its resolved path, as a file is (antigravityPathOwner), so a
// workspace that is a link into an excluded checkout is not collected. One outside git is
// not, unless global mode collects it, as it collects a session outside git. agy reports a
// conversation's first folder as cwd for good, and adds the folder it is resumed from to the
// list, so a conversation resumed from another repository sends no content.
func antigravityWorkspacesCollected(e hookrun.Env, in *antigravityHookInput) bool {
	paths := in.WorkspacePaths
	if len(paths) == 0 {
		paths = []string{e.Cwd}
	}
	for _, p := range paths {
		if p == "" || !filepath.IsAbs(p) {
			return false
		}
		switch antigravityPathOwner(e.Policy, "", p) {
		case ownerCollected:
		case ownerNone:
			if !e.Policy.Global() {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// antigravityOwner is which checkout owns a path.
type antigravityOwner int

const (
	// ownerUnresolved is a path terma cannot place: counted as not collected.
	ownerUnresolved antigravityOwner = iota
	// ownerNone is a path in no git checkout.
	ownerNone
	// ownerCollected is a path in a checkout the policy collects, the hook's own included.
	ownerCollected
	// ownerExcluded is a path in a checkout the policy does not collect.
	ownerExcluded
)

// antigravityPathOwner places path, absolute or relative to cwd: its symlinks resolved (for a
// file not there yet, its nearest existing folder's), then the nearest git checkout that
// holds it, so a nested clone or a submodule is its own, judged by the policy.
//
// With GIT_DIR in the hook's environment, gitx.LocateFS pairs any folder with that git
// directory, so no path's checkout can be told: every path is unresolved, and with every
// workspace unresolved such a session sends no content at all. A path that starts with "~" is
// unresolved too: agy may expand it to a home folder, terma does not.
func antigravityPathOwner(pol config.Policy, cwd, path string) antigravityOwner {
	if os.Getenv("GIT_DIR") != "" || strings.HasPrefix(path, "~") {
		return ownerUnresolved
	}
	if !filepath.IsAbs(path) {
		if cwd == "" {
			return ownerUnresolved
		}
		path = cwd + string(filepath.Separator) + path
	}
	resolved, ok := antigravityResolve(path)
	if !ok {
		return ownerUnresolved
	}
	// A file's checkout is its folder's.
	if info, err := os.Stat(resolved); err != nil {
		return ownerUnresolved
	} else if !info.IsDir() {
		resolved = filepath.Dir(resolved)
	}
	for cur := resolved; ; cur = filepath.Dir(cur) {
		_, err := os.Lstat(filepath.Join(cur, ".git"))
		if err == nil {
			root, gitDir, found := gitx.LocateFS(cur)
			if !found || root != cur {
				return ownerUnresolved
			}
			origin, _ := gitx.OriginFS(gitDir)
			if pol.Admits(config.Repository{Origin: origin}) {
				return ownerCollected
			}
			return ownerExcluded
		}
		if !errors.Is(err, os.ErrNotExist) {
			return ownerUnresolved
		}
		if filepath.Dir(cur) == cur {
			return ownerNone
		}
	}
}

// antigravityResolve is path with its symlinks resolved, or for a path that does not exist
// yet, its nearest existing folder's resolved; false when neither can be had. A missing path
// that climbs with ".." is not resolved: past a link, ".." names another folder than the
// text does. Nor is a path through a dangling link, the path itself or a folder on the way:
// where it leads is unknown, not its folder's.
func antigravityResolve(path string) (string, bool) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, true
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if slices.Contains(strings.Split(filepath.ToSlash(path), "/"), "..") {
		return "", false
	}
	for cur := filepath.Clean(path); ; cur = filepath.Dir(cur) {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return resolved, true
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(cur) == cur {
			return "", false
		}
		// Something there that does not resolve is a link to nothing (or cannot be read).
		if _, err := os.Lstat(cur); !errors.Is(err, os.ErrNotExist) {
			return "", false
		}
	}
}

// antigravityRefuses reports whether the content of the hook's own tool call is refused
// (antigravityRefusesArgs), a relative path judged from the conversation's cwd.
func antigravityRefuses(e hookrun.Env, in *antigravityHookInput) bool {
	return in.ToolCall != nil && antigravityRefusesArgs(e, e.Cwd, in.ToolCall.Args)
}

// antigravityRefusesArgs reports whether a tool call's content is refused: its arguments name
// a file or folder (antigravityContentKeys, any tool) in a checkout the policy does not
// collect, or one terma cannot place, a relative path joined to cwd; a path in no repository
// is refused only when antigravityShowsFilesInNoRepository is false. Arguments terma cannot
// read as an object name no file it knows of. The hook judges its own call's arguments by it,
// and the transcript's reader every call a call line asks for, so both refuse the same calls.
func antigravityRefusesArgs(e hookrun.Env, cwd string, raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil {
		return false
	}
	for _, key := range antigravityContentKeys {
		var p string
		if json.Unmarshal(args[key], &p) != nil || p == "" {
			continue
		}
		switch antigravityPathOwner(e.Policy, cwd, p) {
		case ownerCollected:
		case ownerNone:
			if !antigravityShowsFilesInNoRepository {
				return true
			}
		default:
			return true
		}
	}
	return false
}
