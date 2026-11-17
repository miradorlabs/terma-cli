package codex

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// codexSkillFile is the file a skill's folder holds it in.
const codexSkillFile = "SKILL.md"

// skillReaders are the commands Codex's own parser counts as reading a file: the model
// loads a skill by printing its SKILL.md with one.
var skillReaders = []string{"cat", "bat", "batcat", "less", "more", "head", "tail", "nl", "sed", "awk"}

// shellSkillReads is the SKILL.md files a shell command prints, absolute and cleaned, in
// order, each once: the literal operands of skillReaders named SKILL.md, resolved against cwd
// and any cd before them. A command that does not parse gives none.
func shellSkillReads(command, cwd string) []string {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil
	}
	dir := cwd
	var out []string
	type scope struct {
		node syntax.Node
		dir  string
	}
	var stack []scope
	syntax.Walk(file, func(n syntax.Node) bool {
		if n == nil {
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			dir = leaveScope(top.node, top.dir, dir)
			return true
		}
		switch n := n.(type) {
		case *syntax.FuncDecl:
			return false // defining a function runs nothing
		case *syntax.CallExpr:
			words := wordsOf(n)
			dir = cdTo(words, dir)
			if len(words) == 0 || !slices.Contains(skillReaders, filepath.Base(words[0].text)) || editsInPlace(words) {
				break
			}
			for _, w := range words[1:] {
				if !w.literal || strings.HasPrefix(w.text, "-") || filepath.Base(w.text) != codexSkillFile {
					continue
				}
				p := w.text
				if !filepath.IsAbs(p) {
					if dir == "" {
						continue
					}
					p = filepath.Join(dir, p)
				}
				if p = filepath.Clean(p); !slices.Contains(out, p) {
					out = append(out, p)
				}
			}
		}
		stack = append(stack, scope{n, dir})
		return true
	})
	return out
}

// editsInPlace reports a sed that rewrites its files (-i, BSD's -I, or --in-place): it
// prints no skill, so editing one is not using it. Short options are read letter by letter,
// up to one that takes the rest of the word, or the next word, as its argument: the i in
// `-ne/skill/p` belongs to a script.
func editsInPlace(words []word) bool {
	if filepath.Base(words[0].text) != "sed" {
		return false
	}
	for i := 1; i < len(words); i++ {
		w := words[i].text
		switch {
		case w == "--":
			return false
		case strings.HasPrefix(w, "--"):
			if strings.HasPrefix(w, "--in-place") {
				return true
			}
			if w == "--expression" || w == "--file" {
				i++ // its argument
			}
		case strings.HasPrefix(w, "-"):
			for j, c := range w[1:] {
				if c == 'i' || c == 'I' {
					return true
				}
				if c == 'e' || c == 'f' {
					if j == len(w)-2 {
						i++ // its argument is the next word
					}
					break
				}
			}
		}
	}
	return false
}

// codexSkillRoots are where Codex finds skills outside a repository.
type codexSkillRoots struct {
	// Home is CODEX_HOME; User is the developer's home folder; Managed is the machine's.
	Home, User, Managed string
}

// codexRoots are this machine's.
func codexRoots() codexSkillRoots {
	roots := codexSkillRoots{Managed: filepath.Join(string(filepath.Separator), "etc", "codex", "skills")}
	roots.Home, _ = codexHome()
	roots.User, _ = os.UserHomeDir()
	return roots
}

// codexSkill is the skill whose SKILL.md is at path, when Codex counts it as one: a file
// under one of its skill roots, judged from the path as read, before its symlinks are
// resolved, with frontmatter. Codex names a skill by its frontmatter's name, else its
// folder's, a plugin's prefixed with the plugin's.
func codexSkill(e hookrun.Env, r *hookrun.Repo, path string) (hookrun.Skill, bool) {
	return codexRoots().skill(e, r, path)
}

func (roots codexSkillRoots) skill(e hookrun.Env, r *hookrun.Repo, path string) (hookrun.Skill, bool) {
	var s hookrun.Skill
	below := func(root string, elem ...string) bool {
		_, ok := hookrun.PathBelow(path, root, elem...)
		return ok
	}
	link := false
	switch rest, inPlugins := hookrun.PathBelow(path, roots.Home, "plugins", "cache"); {
	case below(roots.Home, "skills", ".system"):
		s.Origin = semconv.TermaSkillOriginBuiltin
	case below(roots.Home, "skills"), below(roots.User, ".agents", "skills"):
		s.Origin, link = semconv.TermaSkillOriginPersonal, true
	case inPlugins:
		// <marketplace>/<plugin>/<version>/…/SKILL.md
		// Accepted risk: a plugin's skill carries its plugin and marketplace names but no link,
		// because the plugin's place in its marketplace repository was not observed on a real
		// install. Upgrade path: read [marketplaces.<name>] (source_type = "git", source) from
		// config.toml and the manifest in $CODEX_HOME/.tmp/marketplaces/<name>/.
		segs := strings.Split(filepath.ToSlash(rest), "/")
		if len(segs) < 4 {
			return s, false
		}
		s.Origin, s.Marketplace, s.Plugin = semconv.TermaSkillOriginPlugin, segs[0], segs[1]
	case below(roots.Managed):
		s.Origin = semconv.TermaSkillOriginManaged
	case r != nil && repositorySkill(path, r.Root):
		s.Origin, link = semconv.TermaSkillOriginRepository, true
	default:
		return s, false
	}
	if filepath.Base(path) != codexSkillFile {
		return s, false
	}
	fm, ok := hookrun.ReadSkillFrontmatter(path)
	if !ok {
		return s, false
	}
	s.Name = fm.Name
	if s.Name == "" {
		s.Name = filepath.Base(filepath.Dir(path))
	}
	if s.Plugin != "" {
		s.Name = s.Plugin + ":" + s.Name
	}
	s.Description = fm.Description
	if link {
		s.RepositoryURL, s.Path = e.SkillLink(path)
	}
	return s, true
}

// repositorySkill reports whether path lies in a .agents/skills or .codex/skills directory of
// the checkout at root.
func repositorySkill(path, root string) bool {
	rel, ok := hookrun.PathBelow(path, root)
	if !ok {
		return false
	}
	segs := strings.Split(filepath.ToSlash(rel), "/")
	for i := 0; i+2 < len(segs); i++ {
		if (segs[i] == ".agents" || segs[i] == ".codex") && segs[i+1] == "skills" {
			return true
		}
	}
	return false
}
