package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Which content a hook may attach (the package comment says how it fits), and the two rules
// for content of another repository (design 3.3): every workspace in a collected checkout, and
// no content of a call of agy's own file and folder tools that names a path elsewhere. Both ask
// which checkout holds a path: gitx places it, the policy judges the checkout.

// antigravityShowsFilesInNoRepository is the answer for a file in no repository at all (a
// skill file, a note in the home folder): shown, as Claude Code and Codex show such reads.
// false refuses it as a file in a repository the policy does not collect. A folder's answer is
// its own, stricter one (refuses).
var antigravityShowsFilesInNoRepository = true

// antigravityPathTool is what terma knows of one of agy's own file or folder tools: the
// argument keys that name the path it reads, changes, lists or searches, and whether that path
// is a folder.
type antigravityPathTool struct {
	keys   []string
	folder bool
}

// antigravityPathTools are agy's own file and folder tools, by name: the read tools, the edit
// tools (antigravityEditTools) and sed_file, each naming its file under antigravityPathKeys or
// view_file's AbsolutePath, and the tools that list or search a folder. Only a call of one of
// them is judged (refuses); a tool is added here when terma learns its shape.
var antigravityPathTools = func() map[string]antigravityPathTool {
	file := antigravityPathTool{keys: append(slices.Clone(antigravityPathKeys), "AbsolutePath")}
	tools := map[string]antigravityPathTool{
		"read_file": file, "view_file": file, "sed_file": file,
		"list_dir":     {keys: []string{"DirectoryPath"}, folder: true},
		"grep_search":  {keys: []string{"SearchPath"}, folder: true},
		"find_by_name": {keys: []string{"SearchDirectory"}, folder: true},
	}
	for name := range antigravityEditTools {
		tools[name] = file
	}
	return tools
}()

// contentConsented reports whether the developer chose Antigravity, or global mode collects
// every session: what was said travels under it, the team's policy deciding which kinds.
func contentConsented(c hookrun.Consent) bool {
	return c.Global || slices.Contains(c.Agents, Agent{}.Name())
}

// antigravityContent is which kinds of content a hook may attach: each needs its policy
// switch, the developer's consent, and every workspace in a checkout the policy collects.
type antigravityContent struct{ prompts, tools bool }

// antigravityContentFor is the content the hook in e (its Cwd where it ran) may attach to
// what it records in r, its workspaces placed by paths: the switches are those of the policy
// r's records leave under, the routed team's once routing is active (hookrun.Env.PolicyFor).
func antigravityContentFor(e hookrun.Env, r *hookrun.Repo, in *antigravityHookInput, paths *antigravityPaths) antigravityContent {
	prompts, tools := e.PolicyFor(r).Content()
	if !prompts && !tools || !contentConsented(e.Consent(r)) || !paths.workspacesCollected(e.Cwd, in) {
		return antigravityContent{}
	}
	return antigravityContent{prompts: prompts, tools: tools}
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

// antigravityPaths judges paths for one hook, and remembers the owner of each resolved folder
// it has placed, for the life of the hook. A checkout is collected when its records leave
// with those of the hook's own checkout r (hookrun.Env.RoutesWith): the policy admits it
// before routing, and with it routing sends both to the same team.
type antigravityPaths struct {
	e      hookrun.Env
	r      *hookrun.Repo
	owners map[string]antigravityOwner
}

// newAntigravityPaths judges paths for the hook in e, whose checkout is r, with nothing
// placed yet.
func newAntigravityPaths(e hookrun.Env, r *hookrun.Repo) *antigravityPaths {
	return &antigravityPaths{e: e, r: r, owners: map[string]antigravityOwner{}}
}

// global reports whether a folder outside git is collected with the hook's own checkout:
// global mode before routing, and with it a team that takes both.
func (p *antigravityPaths) global() bool { return p.e.RoutesWith(p.r, "", "") }

// owner places path, absolute or relative to cwd: the checkout that holds its resolved folder
// (gitx.ResolveFolderFS, gitx.PlaceFolderFS), judged by the policy. A path that starts with "~"
// is unresolved: agy may expand it to a home folder, terma does not. Not filepath.Join, which
// would clean away a ".." the resolution refuses. With GIT_DIR in the hook's environment every
// path is unresolved, and with every workspace unresolved such a session sends no content.
func (p *antigravityPaths) owner(cwd, path string) antigravityOwner {
	switch {
	case strings.HasPrefix(path, "~"):
		return ownerUnresolved
	case !filepath.IsAbs(path) && cwd == "":
		return ownerUnresolved
	case !filepath.IsAbs(path):
		path = cwd + string(filepath.Separator) + path
	}
	dir, ok := gitx.ResolveFolderFS(path)
	if !ok {
		return ownerUnresolved
	}
	if owner, ok := p.owners[dir]; ok {
		return owner
	}
	owner := ownerUnresolved
	switch _, gitDir, place := gitx.PlaceFolderFS(dir); place {
	case gitx.NoCheckout:
		owner = ownerNone
	case gitx.InCheckout:
		owner = ownerExcluded
		if origin, _ := gitx.OriginFS(gitDir); p.e.RoutesWith(p.r, gitDir, origin) {
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
			if !p.global() {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// refuses reports whether the content of a call of tool name with arguments raw is refused.
// Only a tool of antigravityPathTools is judged, by the strings under its keys, a relative one
// joined to cwd: the call is refused when one names a path in a checkout the policy does not
// collect, or one terma cannot place. A file in no repository is shown unless
// antigravityShowsFilesInNoRepository is false; a folder there is refused outside global mode,
// since a listing or a search of it returns what is under it. A call of such a tool whose
// arguments terma cannot judge is refused: none, not an object, a key's value not a string, or
// no path under any of its keys.
//
// Every other tool's arguments are not judged: an MCP tool's, a shell command's, a subagent's.
// They go under the session's consent, as Claude Code's and Codex's do. The hook judges its
// own call by this rule, and the transcript's reader every call a call line asks for.
func (p *antigravityPaths) refuses(cwd, name string, raw json.RawMessage) bool {
	tool, judged := antigravityPathTools[name]
	if !judged {
		return false
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil || args == nil {
		return true
	}
	named := false
	for _, key := range tool.keys {
		value, ok := args[key]
		if !ok || string(value) == "null" {
			continue
		}
		var path string
		if json.Unmarshal(value, &path) != nil {
			return true
		}
		if path == "" {
			continue
		}
		named = true
		switch p.owner(cwd, path) {
		case ownerCollected:
		case ownerNone:
			if tool.folder && !p.global() || !tool.folder && !antigravityShowsFilesInNoRepository {
				return true
			}
		default:
			return true
		}
	}
	return !named
}
