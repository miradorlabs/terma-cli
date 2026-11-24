package antigravity

import (
	"cmp"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// antigravitySkillFile is the file a skill's folder holds it in.
const antigravitySkillFile = "SKILL.md"

// antigravityRepositorySkillFolders are the folders of a checkout agy reads skills from.
var antigravityRepositorySkillFolders = []string{".agents", ".agent", "_agents", "_agent"}

// antigravitySkillRead is the SKILL.md a successful read_file read, absolute and cleaned: how
// the model loads a skill agy lists, a relative Path joined to cwd. "" for any other step,
// and for a Path with a ".." in it: past a linked folder it names another file than the
// cleaned path does.
//
// Accepted risk: agy before 1.5 read files with view_file and AbsolutePath, which was never
// captured loading a skill, so such a read is not counted. Upgrade path: a capture of an older
// agy loading one, then view_file beside read_file here.
func antigravitySkillRead(in *antigravityHookInput, cwd string) string {
	if in.ToolCall == nil || in.ToolCall.Name != "read_file" || in.Error != "" {
		return ""
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(in.ToolCall.Args, &args) != nil {
		return ""
	}
	var p string
	if json.Unmarshal(args["Path"], &p) != nil || p == "" || filepath.Base(p) != antigravitySkillFile ||
		slices.Contains(strings.Split(filepath.ToSlash(p), "/"), "..") {
		return ""
	}
	if !filepath.IsAbs(p) {
		if cwd == "" {
			return ""
		}
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

// antigravitySkillRoots are where agy finds skills outside a repository for the session whose
// payload is in: the folders of its own Antigravity home, ~/.gemini for the agy on PATH and a
// client's own for an agy it runs (homes.go). They are the skills agy ships,
// <appDataDir>/builtin, and the home's global customization root, config, beside the app data
// folder. The home is read off the conversation's transcript, <appDataDir>/brain/<id>/...; nil
// when the payload names none (antigravityTranscript), as a name could not go then anyway,
// without the conversation's lock.
func antigravitySkillRoots(in *antigravityHookInput) []string {
	transcript := antigravityTranscript(in)
	if transcript == "" {
		return nil
	}
	// antigravityTranscript vouches for the five names below the app data folder.
	appData := transcript
	for range 5 {
		appData = filepath.Dir(appData)
	}
	return []string{filepath.Join(appData, "builtin"), filepath.Join(filepath.Dir(appData), "config")}
}

// antigravityUsedSkill is the name of the skill this step loaded: a read_file of a SKILL.md
// where agy keeps skills for this session, from its own home (antigravitySkillRoots) or the
// checkout at repoRoot ("" outside one) seen from cwd. The caller decides whether the name may
// go; nothing is read from disk unless the step read a SKILL.md.
func antigravityUsedSkill(in *antigravityHookInput, repoRoot, cwd string) (name string, ok bool) {
	path := antigravitySkillRead(in, cwd)
	if path == "" {
		return "", false
	}
	return antigravitySkillName(antigravitySkillRoots(in), repoRoot, cwd, path)
}

// antigravitySkillName is the name of the skill whose SKILL.md is at path, when agy counts it
// as one: a file exactly one folder below a skills folder of one of roots, or of the checkout
// at repoRoot ("" outside one), or of a plugin's folder there, judged from the path as read,
// before its symlinks are resolved, with frontmatter YAML accepts. The first root path lies
// below decides: a path below one that does not match the layout is not a skill. The name is
// the frontmatter's, else the folder's; a plugin's skill is plugin:name.
//
// Accepted risk: a skill skills.json or plugins.json names lies under no root and is not
// reported. Upgrade path: read those files' folders as roots.
func antigravitySkillName(roots []string, repoRoot, cwd, path string) (string, bool) {
	plugin, ok := antigravitySkillPlace(roots, repoRoot, cwd, path)
	if !ok {
		return "", false
	}
	fmName, read := antigravitySkillFrontmatterName(path)
	if !read {
		return "", false
	}
	name := cmp.Or(fmName, filepath.Base(filepath.Dir(path)))
	if plugin != "" && !strings.HasPrefix(name, plugin+":") {
		name = plugin + ":" + name
	}
	return name, true
}

// antigravitySkillPlace reports whether path is where agy keeps a skill, and the plugin it
// belongs to: under the first of roots it lies below, else in the checkout at repoRoot.
func antigravitySkillPlace(roots []string, repoRoot, cwd, path string) (plugin string, ok bool) {
	for _, root := range roots {
		if rel, below := pathBelow(path, root); below {
			return antigravitySkillLayout(strings.Split(filepath.ToSlash(rel), "/"))
		}
	}
	if repoRoot == "" {
		return "", false
	}
	return antigravityRepositorySkill(path, repoRoot, cwd)
}

// antigravitySkillLayout reports whether segs, a path below a root, is skills/<folder>/SKILL.md,
// or plugins/<plugin>/skills/<folder>/SKILL.md, whose plugin it returns.
func antigravitySkillLayout(segs []string) (plugin string, ok bool) {
	switch {
	case len(segs) == 3 && segs[0] == "skills" && segs[2] == antigravitySkillFile:
		return "", true
	case len(segs) == 5 && segs[0] == "plugins" && segs[2] == "skills" && segs[4] == antigravitySkillFile:
		return segs[1], true
	}
	return "", false
}

// antigravityRepositorySkill reports whether path lies in the checkout at root, in one of
// agy's skill folders, and the plugin it belongs to. agy reads those folders from its working
// directory up to the repository's root, so one in any other folder of the checkout (an
// example's, a dependency's) holds no skill of this session; a cwd outside the checkout counts
// as its root.
func antigravityRepositorySkill(path, root, cwd string) (plugin string, ok bool) {
	rel, below := pathBelow(path, root)
	if !below {
		return "", false
	}
	// "" at the root.
	from, _ := pathBelow(cwd, root)
	from = filepath.ToSlash(from)
	segs := strings.Split(filepath.ToSlash(rel), "/")
	// A plugin's layout first: a plugin's folder may bear a skill folder's name.
	for _, n := range []int{5, 3} {
		i := len(segs) - n - 1
		if i < 0 || !slices.Contains(antigravityRepositorySkillFolders, segs[i]) {
			continue
		}
		if holder := strings.Join(segs[:i], "/"); holder != "" && holder != from && !strings.HasPrefix(from, holder+"/") {
			continue
		}
		if plugin, ok := antigravitySkillLayout(segs[i+1:]); ok {
			return plugin, true
		}
	}
	return "", false
}

// pathBelow is path relative to root, when path lies below it. path is judged as written,
// before its symlinks are resolved, since whether a file is a skill is decided by where agy
// found it; root is compared both as given and symlink-resolved (/tmp is /private/tmp on
// macOS).
func pathBelow(path, root string) (string, bool) {
	if root == "" {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolved = root
	}
	for _, base := range []string{root, resolved} {
		rel, err := filepath.Rel(base, path)
		if err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return rel, true
		}
	}
	return "", false
}

// nameSkill puts the skill a step loaded on its terma.tool.call, when its name is text a
// record can carry: valid UTF-8 within the attribute bound.
func nameSkill(call map[string]any, name string) {
	if utf8.ValidString(name) {
		hookrun.BoundedAttr(call, semconv.TermaSkillNameKey, name)
	}
}
