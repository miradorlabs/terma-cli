package antigravity

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Which content a hook may attach (the package comment says how it fits), and the two rules
// for content of another repository (design 3.3). Both ask which checkout owns a path (gitx
// places it, the policy judges the checkout); for a folder a tool names, also which checkouts
// it holds (gitx.NestedCheckoutsFS).

// antigravityShowsFilesInNoRepository is the answer for a file in no repository at all (a
// skill file, a note in the home folder): shown, as Claude Code and Codex show such reads.
// false refuses it as a file in a repository the policy does not collect. It decides every
// path but a folder's, which has its own, stricter rule (refusesPath).
var antigravityShowsFilesInNoRepository = true

// antigravityFileKeys are the argument keys known to name a file, for any tool: the edit
// tools' (antigravityPathKeys) and view_file's. A relative value under one of them, or under
// antigravityFolderKeys, is always judged; under any other key, by its form
// (antigravityLooksLikePath). What a value names, a file or a folder, is never read from its
// key: a file key that names a folder is a folder.
var antigravityFileKeys = append(slices.Clone(antigravityPathKeys), "AbsolutePath")

// antigravityFolderKeys are the argument keys that name a folder a tool lists, searches or
// runs in (list_dir, grep_search, find_by_name, and run_command where a version sends Cwd).
// They say one thing only a key can: a value under one that is not there yet names a folder,
// not a file (refusesPath).
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
	// holds remembers, per resolved folder, whether it holds a checkout the policy does not
	// collect, or one terma cannot tell (holdsExcluded).
	holds map[string]bool
	// placed counts the folders placed, the lookups the cache saved excluded; walked the
	// folders walked.
	placed, walked int
}

// newAntigravityPaths judges paths under pol, with nothing placed yet.
func newAntigravityPaths(pol config.Policy) *antigravityPaths {
	return &antigravityPaths{pol: pol, owners: map[string]antigravityOwner{}, holds: map[string]bool{}}
}

// owner places path, absolute or relative to cwd: the checkout that holds its resolved folder
// (gitx.ResolveFolderFS, gitx.PlaceFolderFS), judged by the policy. A path that starts with "~"
// is unresolved: agy may expand it to a home folder, terma does not. With GIT_DIR in the
// hook's environment every path is unresolved, and with every workspace unresolved such a
// session sends no content at all.
func (p *antigravityPaths) owner(cwd, path string) antigravityOwner {
	owner, _ := p.place(antigravityAbsolute(cwd, path))
	return owner
}

// antigravityAbsolute is path joined to cwd when it is relative, or "" when it cannot be
// placed: it starts with "~", or is relative with no cwd. Not filepath.Join, which would
// clean away a ".." the resolution refuses.
func antigravityAbsolute(cwd, path string) string {
	switch {
	case strings.HasPrefix(path, "~"):
		return ""
	case filepath.IsAbs(path):
		return path
	case cwd == "":
		return ""
	}
	return cwd + string(filepath.Separator) + path
}

// place is the owner of path, absolute or "" (antigravityAbsolute), and the resolved folder
// it was placed by, "" when it was not.
func (p *antigravityPaths) place(path string) (antigravityOwner, string) {
	if path == "" {
		return ownerUnresolved, ""
	}
	dir, ok := gitx.ResolveFolderFS(path)
	if !ok {
		return ownerUnresolved, ""
	}
	if owner, ok := p.owners[dir]; ok {
		return owner, dir
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
	return owner, dir
}

// holdsExcluded reports whether dir, a resolved folder in a checkout the policy collects,
// holds a checkout nested below it that the policy does not collect: a clone or a submodule
// (gitx.NestedCheckoutsFS). A tool that lists or searches dir returns what is under it. A
// nested checkout terma cannot place counts as excluded, and so does a walk that cannot read
// a folder or passes gitx.NestedWalkLimit folders: what the rest holds is unknown. The answer
// is remembered per folder for the life of the hook, as owners are.
func (p *antigravityPaths) holdsExcluded(dir string) bool {
	if held, ok := p.holds[dir]; ok {
		return held
	}
	p.walked++
	held := false
	err := gitx.NestedCheckoutsFS(dir, func(_, gitDir string) bool {
		origin := ""
		if gitDir != "" {
			origin, _ = gitx.OriginFS(gitDir)
		}
		held = gitDir == "" || !p.pol.Admits(config.Repository{Origin: origin})
		return !held
	})
	held = held || err != nil
	p.holds[dir] = held
	return held
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
//     a relative one joined to cwd;
//   - every file:// URI, under any key, by the path it names (antigravityFileURI);
//   - every object key that is absolute, starts with "~" or is a file:// URI, as a value under
//     a key no tool is known by is: a tool may key a map by the files it names;
//   - every value within a string that holds JSON, an object or a list, walked as if it were
//     nested, to antigravityJSONDepth such strings within one another: deeper, the call is
//     refused. A string that does not parse as JSON is text.
//
// Past antigravityPathsPerCall values judged, nested JSON included, the call is refused
// unplaced.
//
// What a value names is decided by what is there (os.Stat, links followed), never by its key.
// A folder that is there is refused, but in global mode, which collects everything, when it
// is in no repository, or when it is in a collected checkout but holds one the policy does
// not collect below it (holdsExcluded): a tool that lists or searches a folder returns what is
// under it. A value that is not there yet is a file, but under a folder key
// (antigravityFolderKeys), which a tool may name a folder by before it makes it: that is the
// one thing a key decides. A file in no repository is refused only when
// antigravityShowsFilesInNoRepository is false. A value that merely looks like a path in free
// text (an edit's Replacement of "/api/v1/users") resolves to its nearest existing folder,
// which is in no repository, and is shown; a text that happens to name a real excluded
// checkout, or a folder above one, is refused, which errs toward sending less.
//
// Arguments terma cannot read as an object name no file it knows of: the hook attaches none of
// them, and the transcript's reader refuses them itself (antigravityCallsRefused says where the
// two differ). The hook judges its own call's arguments by this rule, and the reader every call
// a call line asks for.
func (p *antigravityPaths) refuses(cwd string, raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil {
		return false
	}
	return (&antigravityArgs{p: p, cwd: cwd}).refusesValue("", args)
}

// antigravityJSONDepth bounds how many strings holding JSON, one within another, a call's
// arguments are walked through.
const antigravityJSONDepth = 3

// antigravityArgs is the judging of one call's arguments: what it has judged so far.
type antigravityArgs struct {
	p   *antigravityPaths
	cwd string
	// judged counts the values judged as paths so far; depth the strings holding JSON entered.
	judged, depth int
}

// refusesValue reports whether v, the value under key, or any value or object key within it is
// refused; the items of a list, and the values of a string holding JSON, are judged under the
// key that holds them.
func (a *antigravityArgs) refusesValue(key string, v any) bool {
	switch v := v.(type) {
	case map[string]any:
		for k, item := range v {
			if antigravityKeyLooksLikePath(k) && a.refusesPath("", k) {
				return true
			}
			if a.refusesValue(k, item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if a.refusesValue(key, item) {
				return true
			}
		}
	case string:
		if nested, ok := antigravityJSONIn(v); ok {
			if a.depth >= antigravityJSONDepth {
				return true
			}
			a.depth++
			defer func() { a.depth-- }()
			return a.refusesValue(key, nested)
		}
		return a.refusesPath(key, v)
	}
	return false
}

// antigravityJSONIn is the object or list a string holds as JSON, when it does: trimmed, it
// starts with "{" or "[" and parses whole.
func antigravityJSONIn(s string) (any, bool) {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "{") && !strings.HasPrefix(t, "[") {
		return nil, false
	}
	var v any
	if json.Unmarshal([]byte(t), &v) != nil {
		return nil, false
	}
	return v, true
}

// antigravityKeyLooksLikePath reports whether an object key is judged as a path: absolute, the
// home folder or a path in it, or a file:// URI. Not the relative rules, which would cost an
// os.Lstat per key.
func antigravityKeyLooksLikePath(k string) bool {
	_, uri := antigravityFileURI(k)
	return uri || filepath.IsAbs(k) || k == "~" || strings.HasPrefix(k, "~/")
}

// antigravityFileURI is the absolute path a file:// URI names, and whether s is one: a single
// line that starts with "file://", any case. The path is "" for one terma cannot place: it
// does not parse as a URL, names a host but localhost, or names no absolute path here.
func antigravityFileURI(s string) (string, bool) {
	if len(s) < len("file://") || !strings.EqualFold(s[:len("file://")], "file://") || strings.ContainsAny(s, "\r\n") {
		return "", false
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "file" || u.Host != "" && !strings.EqualFold(u.Hostname(), "localhost") || u.Port() != "" {
		return "", true
	}
	p := u.Path
	// file:///C:/dir names C:/dir on Windows.
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' && filepath.VolumeName(p[1:]) != "" {
		p = p[1:]
	}
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) {
		return "", true
	}
	return p, true
}

// refusesPath reports whether one string value under key is refused, by refuses's rules.
func (a *antigravityArgs) refusesPath(key, value string) bool {
	p, cwd := a.p, a.cwd
	path, uri := antigravityFileURI(value)
	folderKey := slices.Contains(antigravityFolderKeys, key)
	named := uri || folderKey || slices.Contains(antigravityFileKeys, key)
	if value == "" || !named && !antigravityLooksLikePath(cwd, value) {
		return false
	}
	if a.judged++; a.judged > antigravityPathsPerCall {
		return true
	}
	if !uri {
		path = antigravityAbsolute(cwd, value)
	}
	owner, dir := p.place(path)
	if owner != ownerCollected && owner != ownerNone {
		return true
	}
	info, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return true
	}
	isDir := err == nil && info.IsDir()
	// Global mode collects every repository on the machine and a session outside git, so
	// there a folder holds nothing the policy excludes.
	switch {
	case owner == ownerCollected:
		return isDir && !p.pol.Global() && p.holdsExcluded(dir)
	case isDir || err != nil && folderKey:
		// A folder in no repository may hold several: an MCP tool lists or searches one
		// under "path".
		return !p.pol.Global()
	}
	return !antigravityShowsFilesInNoRepository
}

// antigravityLooksLikePath reports whether an argument value looks like a path whatever its
// key: one line of at most antigravityPathLimit bytes that is absolute, the home folder or a
// path in it ("~", "~/..."), or relative to cwd and either climbing out of it (a segment that
// is exactly "..", split on "/" and on the system's separator) or naming something there
// (os.Lstat). A tool terma has never seen may name "../other-repo/file.txt" under a key of its
// own.
//
// The cost: any word under any key that happens to name a file or folder in cwd is judged as
// a path. cwd is the turn's own checkout, so such a file is collected and shown, but such a
// folder is refused when it holds a checkout the policy does not collect. Other relative text,
// a command line among it ("cd ../x && ls" has no segment that is exactly ".."), is not a path.
func antigravityLooksLikePath(cwd, s string) bool {
	if len(s) > antigravityPathLimit || strings.Contains(s, "\n") {
		return false
	}
	if filepath.IsAbs(s) || s == "~" || strings.HasPrefix(s, "~/") {
		return true
	}
	if slices.Contains(strings.FieldsFunc(s, func(r rune) bool { return r == '/' || r == filepath.Separator }), "..") {
		return true
	}
	if cwd == "" {
		return false
	}
	_, err := os.Lstat(filepath.Join(cwd, s))
	return err == nil
}
