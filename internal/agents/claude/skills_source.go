package claude

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// claudeSkillFile is the file a skill's directory holds it in.
const claudeSkillFile = "SKILL.md"

// githubRepo is a GitHub marketplace's `owner/name`; neither is "." or "..", checked apart.
var githubRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// claudeSkillRoots are where Claude Code keeps skills outside a repository: its config
// directory (personal skills and plugins) and the managed-settings directory.
type claudeSkillRoots struct {
	Config, Managed string
}

// claudeRoots are this machine's: the config directory is the user settings file's,
// CLAUDE_CONFIG_DIR or ~/.claude.
func claudeRoots() claudeSkillRoots {
	roots := claudeSkillRoots{Managed: filepath.Dir(Agent{}.ManagedHookFiles(string(filepath.Separator))[0])}
	if settings, err := (exporter{}).ConfigPath(); err == nil {
		roots.Config = filepath.Dir(settings)
	}
	return roots
}

// claudeSkillSource is where the skill loaded from dir is held, on this machine's roots.
func claudeSkillSource(e hookrun.Env, r *hookrun.Repo, dir string) hookrun.Skill {
	return claudeRoots().source(e, r, dir)
}

// source tells a skill's origin from the directory Claude Code loaded it from, as written,
// before its own symlinks are resolved: a personal skill is often a link into a checkout.
// Only a skill in a checkout the team's policy collects, or in a plugin's GitHub
// marketplace, says where its file is held.
func (roots claudeSkillRoots) source(e hookrun.Env, r *hookrun.Repo, dir string) hookrun.Skill {
	var s hookrun.Skill
	if dir == "" || !filepath.IsAbs(dir) {
		return s
	}
	dir = filepath.Clean(dir)
	file := filepath.Join(dir, claudeSkillFile)
	if rest, ok := hookrun.PathBelow(dir, roots.Config, "plugins", "cache"); ok {
		// <marketplace>/<plugin>/<version>/<the skill's place in the plugin>
		segs := strings.Split(filepath.ToSlash(rest), "/")
		if len(segs) < 4 {
			return s
		}
		s.Origin, s.Marketplace, s.Plugin = semconv.TermaSkillOriginPlugin, segs[0], segs[1]
		s.RepositoryURL, s.Path = roots.pluginLink(segs[0], segs[1], path.Join(append(segs[3:], claudeSkillFile)...))
		return s
	}
	switch {
	case inside(dir, roots.Config, "skills"):
		s.Origin = semconv.TermaSkillOriginPersonal
		s.RepositoryURL, s.Path = e.SkillLink(file)
	case inside(dir, roots.Managed, ".claude", "skills"):
		s.Origin = semconv.TermaSkillOriginManaged
	case r != nil && r.Root != "" && repositorySkill(dir, r.Root):
		s.Origin = semconv.TermaSkillOriginRepository
		s.RepositoryURL, s.Path = e.SkillLink(file)
	case slices.Contains(strings.Split(filepath.ToSlash(dir), "/"), "bundled-skills"):
		s.Origin = semconv.TermaSkillOriginBuiltin
	}
	return s
}

// inside reports whether dir lies below root joined with elem.
func inside(dir, root string, elem ...string) bool {
	_, ok := hookrun.PathBelow(dir, root, elem...)
	return ok
}

// repositorySkill reports whether dir lies in a .claude/skills directory of the checkout at root.
func repositorySkill(dir, root string) bool {
	rel, ok := hookrun.PathBelow(dir, root)
	if !ok {
		return false
	}
	segs := strings.Split(filepath.ToSlash(rel), "/")
	for i := 0; i+2 < len(segs); i++ {
		if segs[i] == ".claude" && segs[i+1] == "skills" {
			return true
		}
	}
	return false
}

// pluginLink is where a plugin's skill file is held when its marketplace is a GitHub
// repository: that repository, and the file's path in it when that is established, the
// plugin's source directory joined with file, the skill's path in the plugin. A path is given
// only for a file the marketplace's checkout holds, so it is the one the repository has; a
// plugin held elsewhere (a source given as an object), or one the checkout does not show,
// gets the repository alone.
func (roots claudeSkillRoots) pluginLink(marketplace, plugin, file string) (repositoryURL, filePath string) {
	plugins := filepath.Join(roots.Config, "plugins")
	known, status := hookrun.ReadEvidenceJSON(filepath.Join(plugins, "known_marketplaces.json"))
	if status != hookrun.StatusPresent {
		return "", ""
	}
	var entry struct {
		Source struct {
			Source string `json:"source"`
			Repo   string `json:"repo"`
		} `json:"source"`
	}
	if json.Unmarshal(known[marketplace], &entry) != nil || entry.Source.Source != "github" || !githubRepo.MatchString(entry.Source.Repo) ||
		slices.ContainsFunc(strings.Split(entry.Source.Repo, "/"), func(seg string) bool { return strings.Trim(seg, ".") == "" }) {
		return "", ""
	}
	repositoryURL = "https://github.com/" + entry.Source.Repo
	checkout := filepath.Join(plugins, "marketplaces", marketplace)
	manifest, status := hookrun.ReadEvidenceJSON(filepath.Join(checkout, ".claude-plugin", "marketplace.json"))
	if status != hookrun.StatusPresent {
		return repositoryURL, ""
	}
	var listed []struct {
		Name   string          `json:"name"`
		Source json.RawMessage `json:"source"`
	}
	if json.Unmarshal(manifest["plugins"], &listed) != nil {
		return repositoryURL, ""
	}
	for _, p := range listed {
		var source string
		// A source given as an object is a plugin held elsewhere: no path.
		if p.Name != plugin || json.Unmarshal(p.Source, &source) != nil {
			continue
		}
		source = path.Clean(strings.TrimPrefix(source, "./"))
		rel := path.Join(source, file)
		if strings.HasPrefix(source, "/") || source == ".." || strings.HasPrefix(source, "../") || strings.Contains(source, `\`) {
			return repositoryURL, ""
		}
		if st, err := os.Lstat(filepath.Join(checkout, filepath.FromSlash(rel))); err != nil || !st.Mode().IsRegular() {
			return repositoryURL, ""
		}
		return repositoryURL, rel
	}
	return repositoryURL, ""
}
