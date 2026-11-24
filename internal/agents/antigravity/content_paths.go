package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Which content a hook may attach (the package comment says how it fits), and the two rules
// for content of another repository (design 3.3). Both ask one question of a path: which
// checkout owns it (gitx places it, the policy judges the checkout).

// antigravityShowsFilesInNoRepository is the answer for a file in no repository at all (a
// skill file, a note in the home folder): shown, as Claude Code and Codex show such reads.
// false refuses it as a file in a repository the policy does not collect. It decides every
// path but a folder key's (antigravityFolderKeys), which has its own, stricter rule.
var antigravityShowsFilesInNoRepository = true

// antigravityFileKeys are the argument keys known to name a file, for any tool: the edit
// tools' (antigravityPathKeys) and view_file's. A relative path is judged only under one of
// them or antigravityFolderKeys.
var antigravityFileKeys = append(slices.Clone(antigravityPathKeys), "AbsolutePath")

// antigravityFolderKeys are the argument keys that name a folder a tool lists, searches or
// runs in (list_dir, grep_search, find_by_name, and run_command where a version sends Cwd).
// A folder in no repository may hold several: a search of one returns their files.
var antigravityFolderKeys = []string{"DirectoryPath", "SearchPath", "SearchDirectory", "Cwd"}

// contentConsented reports whether the developer chose Antigravity, or global mode collects
// every session: what was said travels under it, the team's policy deciding which kinds.
func contentConsented(c hookrun.Consent) bool {
	return c.Global || slices.Contains(c.Agents, Agent{}.Name())
}

// antigravityContent is which kinds of content a hook may attach: each needs its policy
// switch, the developer's consent, and every workspace in a checkout the policy collects.
type antigravityContent struct{ prompts, tools bool }

// antigravityContentFor is the content the hook in e (its Cwd where it ran) may attach, its
// workspaces placed by paths.
func antigravityContentFor(e hookrun.Env, in *antigravityHookInput, paths *antigravityPaths) antigravityContent {
	prompts, tools := e.Policy.Content()
	if !prompts && !tools || !contentConsented(e.Consent()) || !paths.workspacesCollected(e.Cwd, in) {
		return antigravityContent{}
	}
	return antigravityContent{prompts: prompts, tools: tools}
}

// antigravityPathLimit is the longest argument value judged as a path when no key says it
// names one.
const antigravityPathLimit = 4096

// antigravityPathsPerCall bounds the values one call's arguments are judged for as paths: each
// may cost a walk of the file system and a read of a checkout's config. A call that names more
// files than terma will place is refused, as one it cannot place is. No tool seen names more
// than a few; a search's or a batch edit's list stays well under it.
const antigravityPathsPerCall = 64

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

// antigravityPaths judges paths for one hook under its policy, and remembers the owner of each
// resolved folder it has placed: a call line with many paths in one folder, and the hook and
// the transcript's reader judging the same arguments, place that folder once.
type antigravityPaths struct {
	pol    config.Policy
	owners map[string]antigravityOwner
	// placed counts the folders placed, the lookups the cache saved excluded.
	placed int
}

// newAntigravityPaths judges paths under pol, with nothing placed yet.
func newAntigravityPaths(pol config.Policy) *antigravityPaths {
	return &antigravityPaths{pol: pol, owners: map[string]antigravityOwner{}}
}

// owner places path, absolute or relative to cwd: the checkout that holds its resolved folder
// (gitx.ResolveFolderFS, gitx.PlaceFolderFS), judged by the policy. A path that starts with "~"
// is unresolved: agy may expand it to a home folder, terma does not. With GIT_DIR in the
// hook's environment every path is unresolved, and with every workspace unresolved such a
// session sends no content at all.
func (p *antigravityPaths) owner(cwd, path string) antigravityOwner {
	if strings.HasPrefix(path, "~") {
		return ownerUnresolved
	}
	if !filepath.IsAbs(path) {
		if cwd == "" {
			return ownerUnresolved
		}
		// Not filepath.Join, which would clean away a ".." the resolution refuses.
		path = cwd + string(filepath.Separator) + path
	}
	dir, ok := gitx.ResolveFolderFS(path)
	if !ok {
		return ownerUnresolved
	}
	if owner, ok := p.owners[dir]; ok {
		return owner
	}
	p.placed++
	owner := ownerUnresolved
	switch _, gitDir, place := gitx.PlaceFolderFS(dir); place {
	case gitx.NoCheckout:
		owner = ownerNone
	case gitx.InCheckout:
		owner = ownerExcluded
		if origin, _ := gitx.OriginFS(gitDir); p.pol.Admits(config.Repository{Origin: origin}) {
			owner = ownerCollected
		}
	}
	p.owners[dir] = owner
	return owner
}

// workspacesCollected reports whether every one of the conversation's workspaces
// (workspacePaths, cwd when the list is empty) is in a checkout the policy collects, judged by
// the owner of its resolved path, as a file is, so a workspace that is a link into an
// excluded checkout is not collected. A workspace must be an existing folder: one removed is
// not judged by the folder above it, which may be the collected checkout an excluded clone
// was nested in. One outside git is not, unless global mode collects it, as it collects a
// session outside git. agy reports a conversation's first folder as cwd for good, and adds
// the folder it is resumed from to the list, so a conversation resumed from another
// repository sends no content.
func (p *antigravityPaths) workspacesCollected(cwd string, in *antigravityHookInput) bool {
	workspaces := in.WorkspacePaths
	if len(workspaces) == 0 {
		workspaces = []string{cwd}
	}
	for _, w := range workspaces {
		if w == "" || !filepath.IsAbs(w) {
			return false
		}
		if info, err := os.Stat(w); err != nil || !info.IsDir() {
			return false
		}
		switch p.owner("", w) {
		case ownerCollected:
		case ownerNone:
			if !p.pol.Global() {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// refuses reports whether a tool call's content is refused, any tool: a value anywhere in its
// arguments, nested objects and lists included, names a file or folder in a checkout the
// policy does not collect, or one terma cannot place. Which values are judged:
//
//   - every one that looks like a path (antigravityLooksLikePath), under any key: what an MCP
//     filesystem tool or a tool agy adds names a file with is not known in advance;
//   - every one under a key known to name a path (antigravityFileKeys, antigravityFolderKeys),
//     a relative one joined to cwd. Elsewhere a relative value is text, not judged.
//
// Past antigravityPathsPerCall values judged, the call is refused unplaced.
//
// A folder in no repository is refused, but in global mode: one that is there, under any
// key, and any value of a folder key. A file in no repository is refused only when
// antigravityShowsFilesInNoRepository is false. A value that
// merely looks like a path in free text (an edit's Replacement of "/api/v1/users") resolves to
// its nearest existing folder, which is in no repository, and is shown; a text that happens to
// name a real excluded checkout is refused, which errs toward sending less.
//
// Arguments terma cannot read as an object name no file it knows of: the hook attaches none of
// them, and the transcript's reader refuses them itself (antigravityCallsRefused). The hook
// judges its own call's arguments by it, and the reader every call a call line asks for, so
// both refuse the same calls.
func (p *antigravityPaths) refuses(cwd string, raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil {
		return false
	}
	judged := 0
	return p.refusesValue(cwd, "", args, &judged)
}

// refusesValue reports whether v, the value under key, or any value within it is refused; the
// items of a list are judged under the list's key. judged counts the values judged as paths
// in the call so far.
func (p *antigravityPaths) refusesValue(cwd, key string, v any, judged *int) bool {
	switch v := v.(type) {
	case map[string]any:
		for k, item := range v {
			if p.refusesValue(cwd, k, item, judged) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if p.refusesValue(cwd, key, item, judged) {
				return true
			}
		}
	case string:
		return p.refusesPath(cwd, key, v, judged)
	}
	return false
}

// refusesPath reports whether one string value under key is refused, by refuses's rules.
func (p *antigravityPaths) refusesPath(cwd, key, value string, judged *int) bool {
	folder := slices.Contains(antigravityFolderKeys, key)
	named := folder || slices.Contains(antigravityFileKeys, key)
	if value == "" || !named && !antigravityLooksLikePath(value) {
		return false
	}
	if *judged++; *judged > antigravityPathsPerCall {
		return true
	}
	switch p.owner(cwd, value) {
	case ownerCollected:
		return false
	case ownerNone:
		// A folder in no repository may hold several, whatever key names it: an MCP tool
		// lists or searches one under "path". Global mode collects every repository on the
		// machine and a session outside git, so there it holds nothing the policy excludes.
		if folder || antigravityIsFolder(cwd, value) {
			return !p.pol.Global()
		}
		return !antigravityShowsFilesInNoRepository
	}
	return true
}

// antigravityIsFolder reports whether value, absolute or relative to cwd, names a folder that
// is there, links followed.
func antigravityIsFolder(cwd, value string) bool {
	if !filepath.IsAbs(value) {
		value = filepath.Join(cwd, value)
	}
	info, err := os.Stat(value)
	return err == nil && info.IsDir()
}

// antigravityLooksLikePath reports whether an argument value looks like a path whatever its
// key: one line, at most antigravityPathLimit bytes, absolute, or the home folder or a path
// in it ("~", "~/...").
func antigravityLooksLikePath(s string) bool {
	return len(s) <= antigravityPathLimit && !strings.Contains(s, "\n") &&
		(filepath.IsAbs(s) || s == "~" || strings.HasPrefix(s, "~/"))
}
