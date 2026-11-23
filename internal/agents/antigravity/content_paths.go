package antigravity

import (
	"context"
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

// antigravityWithholdsRepliesAfterRefusal withholds the replies of a turn from a refused
// call on, until that turn's record ends: the model can repeat what it read. false sends
// them, as every other agent's are.
var antigravityWithholdsRepliesAfterRefusal = false

// antigravityContentKeys are the argument keys that name a file, for any tool.
var antigravityContentKeys = append(slices.Clone(antigravityPathKeys), "AbsolutePath")

// contentConsented reports whether the developer chose Antigravity, or global mode collects
// every session: what was said travels under it, the team's policy deciding which kinds.
func contentConsented(c hookrun.Consent) bool {
	return c.Global || slices.Contains(c.Agents, Agent{}.Name())
}

// antigravityContent is which kinds of content a hook may attach: each needs its policy
// switch, the developer's consent, and every workspace in a checkout the policy collects.
type antigravityContent struct{ prompts, tools bool }

// antigravityContentFor is the content the hook in e (its Cwd where it ran) may attach.
func antigravityContentFor(ctx context.Context, e hookrun.Env, in *antigravityHookInput) antigravityContent {
	prompts, tools := e.Policy.Content()
	if !prompts && !tools || !contentConsented(e.Consent()) || !antigravityWorkspacesCollected(ctx, e, in) {
		return antigravityContent{}
	}
	return antigravityContent{prompts: prompts, tools: tools}
}

// antigravityWorkspacesCollected reports whether every one of the conversation's workspaces
// (workspacePaths, the cwd when the list is empty) is in a checkout the policy collects,
// judged as a session there would be: one outside git is not, unless global mode collects
// it. agy reports a conversation's first folder as cwd for good, and adds the folder it is
// resumed from to the list, so a conversation resumed from another repository sends no
// content.
func antigravityWorkspacesCollected(ctx context.Context, e hookrun.Env, in *antigravityHookInput) bool {
	paths := in.WorkspacePaths
	if len(paths) == 0 {
		paths = []string{e.Cwd}
	}
	for _, p := range paths {
		if p == "" || !filepath.IsAbs(p) || !e.Admits(ctx, p) {
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
func antigravityPathOwner(pol config.Policy, cwd, path string) antigravityOwner {
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
// text does.
func antigravityResolve(path string) (string, bool) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, true
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if slices.Contains(strings.Split(filepath.ToSlash(path), "/"), "..") {
		return "", false
	}
	for cur := filepath.Dir(filepath.Clean(path)); ; cur = filepath.Dir(cur) {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return resolved, true
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(cur) == cur {
			return "", false
		}
	}
}

// antigravityRefuses reports whether a tool call's content is refused: its arguments name a
// file (antigravityContentKeys, any tool) in a checkout the policy does not collect, or one
// terma cannot place; a file in no repository is refused only when
// antigravityShowsFilesInNoRepository is false. Arguments terma cannot read as an object
// name no file it knows of.
func antigravityRefuses(e hookrun.Env, in *antigravityHookInput) bool {
	if in.ToolCall == nil || len(in.ToolCall.Args) == 0 {
		return false
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(in.ToolCall.Args, &args) != nil {
		return false
	}
	for _, key := range antigravityContentKeys {
		var p string
		if json.Unmarshal(args[key], &p) != nil || p == "" {
			continue
		}
		switch antigravityPathOwner(e.Policy, e.Cwd, p) {
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
