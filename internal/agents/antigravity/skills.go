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

// antigravitySkillStateDir keeps which skills each conversation reported.
var antigravitySkillStateDir = hookrun.AgentStateDir(Agent{}.Name(), "skills")

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

// antigravitySkillRoots are where agy finds skills outside a repository, in the home the
// session runs in.
type antigravitySkillRoots struct {
	// Config is the home's global customization root, <home>/config; Builtin holds the
	// skills agy ships, in the home's app data folder, <home>/<app data>/builtin. For agy's
	// own home those are ~/.gemini/config and ~/.gemini/antigravity-cli/builtin.
	Config, Builtin string
}

// antigravityRoots are the roots of the home in's session runs in, read from its
// transcript's path, <home>/<app data>/brain/<id>/.system_generated/logs/...; agy's own
// home's for a payload with none (agy before 1.5).
func antigravityRoots(in *antigravityHookInput) antigravitySkillRoots {
	if appData := antigravityAppData(in); appData != "" {
		return antigravitySkillRoots{Config: filepath.Join(filepath.Dir(appData), "config"), Builtin: filepath.Join(appData, "builtin")}
	}
	config, err := defaultHooksDir()
	if err != nil {
		return antigravitySkillRoots{}
	}
	return antigravitySkillRoots{Config: config, Builtin: filepath.Join(filepath.Dir(config), "antigravity-cli", "builtin")}
}

// antigravityUsedSkill is the skill this step loaded, when the team's policy collects prompts
// and tool content: nothing is read from disk otherwise.
func antigravityUsedSkill(e hookrun.Env, r *hookrun.Repo, in *antigravityHookInput) (hookrun.Skill, bool) {
	if !e.SkillsCollected() {
		return hookrun.Skill{}, false
	}
	path := antigravitySkillRead(in, e.Cwd)
	if path == "" {
		return hookrun.Skill{}, false
	}
	return antigravityRoots(in).skill(e, r, path)
}

// skill is the skill whose SKILL.md is at path, when agy counts it as one: a file exactly one
// folder below a skills folder of one of its roots, or of a plugin's folder there, judged from
// the path as read, before its symlinks are resolved, with frontmatter YAML accepts.
//
// Accepted risk: a skill skills.json or plugins.json names lies under no root and is not
// reported. Upgrade path: read those files' folders as roots.
func (roots antigravitySkillRoots) skill(e hookrun.Env, r *hookrun.Repo, path string) (hookrun.Skill, bool) {
	var s hookrun.Skill
	plugin, ok := "", false
	if rel, below := hookrun.PathBelow(path, roots.Builtin); below {
		plugin, ok = antigravitySkillLayout(strings.Split(filepath.ToSlash(rel), "/"))
		s.Origin = semconv.TermaSkillOriginBuiltin
	} else if rel, below := hookrun.PathBelow(path, roots.Config); below {
		plugin, ok = antigravitySkillLayout(strings.Split(filepath.ToSlash(rel), "/"))
		s.Origin = semconv.TermaSkillOriginPersonal
	} else if r != nil {
		plugin, ok = antigravityRepositorySkill(path, r.Root, e.Cwd)
		s.Origin = semconv.TermaSkillOriginRepository
	}
	if !ok {
		return hookrun.Skill{}, false
	}
	fm, read := hookrun.ReadSkillFrontmatter(path)
	if !read {
		return hookrun.Skill{}, false
	}
	s.Name = cmp.Or(fm.Name, filepath.Base(filepath.Dir(path)))
	s.Description = fm.Description
	if plugin != "" {
		// Accepted risk: a plugin's skill carries its plugin's name but no link or marketplace,
		// because no plugin installed with `agy plugin install` was observed. Upgrade path:
		// capture an installed plugin's folder and marketplace record, and link as Codex does.
		s.Origin, s.Plugin = semconv.TermaSkillOriginPlugin, plugin
		if !strings.HasPrefix(s.Name, plugin+":") {
			s.Name = plugin + ":" + s.Name
		}
		return s, true
	}
	// A skill agy ships is held in no repository, whatever checkout its folder lies in.
	if s.Origin != semconv.TermaSkillOriginBuiltin {
		s.RepositoryURL, s.Path = e.SkillLink(path)
	}
	return s, true
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
	rel, below := hookrun.PathBelow(path, root)
	if !below {
		return "", false
	}
	// "" at the root.
	from, _ := hookrun.PathBelow(cwd, root)
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

// antigravitySkillAttrs are the agent's own attributes on a skill's definition.
func antigravitySkillAttrs() map[string]any {
	return map[string]any{semconv.GenAIMainAgentNameKey: antigravityTool, semconv.TermaEvidenceSourceKey: sourceAntigravityHook}
}

// nameSkill puts the skill a step loaded on its terma.tool.call, when its name may leave.
func nameSkill(call map[string]any, s hookrun.Skill) {
	if utf8.ValidString(s.Name) {
		hookrun.BoundedAttr(call, semconv.TermaSkillNameKey, s.Name)
	}
}
