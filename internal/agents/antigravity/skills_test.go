package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// testdata/antigravity_skill_hooks.jsonl is every PostToolUse payload of three real agy
// 1.5.15 conversations (gemini-3.8-flash-low, print mode, 2026-11-23) that loaded skills,
// with the scratch repository's path replaced by /home/dev/widgets, the home folder's by
// /home/dev, and one personal skill's name by qa-personal.
const (
	capturedRepo = "/home/dev/widgets"
	capturedHome = "/home/dev"
)

// skillCapture is a collected repository and a home folder holding the skills the capture
// read, as they were, and the capture's payloads with its paths moved there.
func skillCapture(t *testing.T) (env hookrun.Env, payloads []string) {
	t.Helper()
	root := hookruntest.InitRepo(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// os.UserHomeDir reads USERPROFILE on Windows and HOME elsewhere.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	skill := func(dir, rel, name, description string) {
		hookruntest.WriteFile(t, dir, rel, "---\nname: "+name+"\ndescription: "+description+"\n---\n\nBody of "+name+".\n")
	}
	skill(root, ".agents/skills/qa-described/SKILL.md", "qa-described", "Greets the person in the house style. Use when asked for a house greeting.")
	// An unquoted ": ", which agy repairs and strict YAML refuses.
	skill(root, ".agents/skills/qa-badyaml/SKILL.md", "qa-badyaml", "Says farewell: use when asked for a house farewell.")
	hookruntest.WriteFile(t, root, ".agents/skills/qa-bodyonly/SKILL.md", "# Body only\n\nNo frontmatter, so no skill to agy.\n")
	skill(root, ".agents/skills/qa-dirname/SKILL.md", "qa-fmname", "Counts to three in the house style. Use when asked for a house count.")
	skill(root, ".agents/plugins/qa-plugin/skills/qa-plugged/SKILL.md", "qa-plugged", "Sings the house song. Use when asked for the house song.")
	hookruntest.WriteFile(t, root, ".agents/plugins/qa-plugin/plugin.json", `{"name": "qa-plugin-display"}`)
	// Present, so only the payload's error makes its read a failed use.
	skill(root, ".agents/skills/qa-missing/SKILL.md", "qa-missing", "Never loaded.")
	skill(home, ".gemini/config/skills/qa-personal/SKILL.md", "qa-personal", "Cuts filler from prose.")
	skill(home, ".gemini/antigravity-cli/builtin/skills/antigravity_guide/SKILL.md", "antigravity-guide", "Answers questions about agy.")
	hookruntest.WriteFile(t, home, ".gemini/antigravity-cli/builtin/skills/antigravity_guide/references/cli.md", "---\nname: cli\n---\n")

	data, err := os.ReadFile(filepath.Join("testdata", "antigravity_skill_hooks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.NewReplacer(capturedRepo, hookruntest.InJSON(root), capturedHome, hookruntest.InJSON(home)).Replace(string(data))
	sc := bufio.NewScanner(strings.NewReader(moved))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			payloads = append(payloads, line)
		}
	}
	if len(payloads) != 10 {
		t.Fatalf("the capture has %d payloads, want 10", len(payloads))
	}
	sp, err := hookruntest.OpenSpool(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return hookrun.Env{StateDir: t.TempDir(), Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Spool: sp, Version: "test"}, payloads
}

// replay runs each payload through postToolUse as agy's hook would.
func replay(t *testing.T, env hookrun.Env, payloads []string) {
	t.Helper()
	for _, p := range payloads {
		var stdout bytes.Buffer
		env.Stdin, env.Stdout = strings.NewReader(p), &stdout
		if err := postToolUse(context.Background(), env); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(stdout.String()) != "{}" {
			t.Fatalf("agy expects {} on stdout, got %q", stdout.String())
		}
	}
}

// capturedUses is the skill each captured step loaded, in the capture's order: a read of a
// file YAML refuses, of a body-only file, one that failed, a shell search and a skill's
// reference file load none.
var capturedUses = []string{"qa-described", "", "qa-plugin:qa-plugged", "qa-fmname", "", "", "", "qa-personal", "antigravity-guide", ""}

// checkCalls checks every step's terma.tool.call is sent, each naming the skill it loaded.
func checkCalls(t *testing.T, events []spool.Event, uses []string) {
	t.Helper()
	calls := hookruntest.Named(events, semconv.TermaToolCallEvent)
	if len(calls) != len(uses) {
		t.Fatalf("%d tool calls, want %d", len(calls), len(uses))
	}
	for i, c := range calls {
		got, named := c.Attrs[semconv.TermaSkillNameKey]
		if uses[i] == "" && named || uses[i] != "" && got != uses[i] {
			t.Errorf("call %d (%v): skill %v, want %q", i, c.Attrs[semconv.GenAIToolNameKey], got, uses[i])
		}
	}
}

// Each skill an agy conversation loads is defined once, from its own file, and named on the
// read that loaded it; a read that loaded no skill names none, and every step is still a call.
func TestPostToolUseReportsTheSkillsAgyLoaded(t *testing.T) {
	env, payloads := skillCapture(t)
	replay(t, env, payloads)
	events := hookruntest.Spooled(t, env.Spool)
	checkCalls(t, events, capturedUses)

	repoURL := "https://github.com/acme/" + filepath.Base(env.Cwd)
	agent := map[string]any{semconv.GenAIMainAgentNameKey: antigravityTool, semconv.TermaEvidenceSourceKey: sourceAntigravityHook}
	with := func(attrs map[string]any) map[string]any {
		maps.Copy(attrs, agent)
		return attrs
	}
	want := []struct {
		session string
		attrs   map[string]any
	}{
		{"a4a28843-6d58-4bde-8132-2ce6ad3acac4", with(map[string]any{
			semconv.TermaSkillNameKey: "qa-described", semconv.TermaSkillOriginKey: semconv.TermaSkillOriginRepository,
			semconv.TermaSkillDescriptionKey:   "Greets the person in the house style. Use when asked for a house greeting.",
			semconv.TermaSkillRepositoryURLKey: repoURL, semconv.TermaSkillPathKey: ".agents/skills/qa-described/SKILL.md",
		})},
		{"7e807ab4-8a72-44aa-a0f8-ef76d91a8374", with(map[string]any{
			semconv.TermaSkillNameKey: "qa-plugin:qa-plugged", semconv.TermaSkillOriginKey: semconv.TermaSkillOriginPlugin,
			semconv.TermaSkillPluginNameKey: "qa-plugin", semconv.TermaSkillDescriptionKey: "Sings the house song. Use when asked for the house song.",
		})},
		{"7e807ab4-8a72-44aa-a0f8-ef76d91a8374", with(map[string]any{
			semconv.TermaSkillNameKey: "qa-fmname", semconv.TermaSkillOriginKey: semconv.TermaSkillOriginRepository,
			semconv.TermaSkillDescriptionKey:   "Counts to three in the house style. Use when asked for a house count.",
			semconv.TermaSkillRepositoryURLKey: repoURL, semconv.TermaSkillPathKey: ".agents/skills/qa-dirname/SKILL.md",
		})},
		{"1cc63765-abef-4bc9-97c3-85e0b1a9e714", with(map[string]any{
			semconv.TermaSkillNameKey: "qa-personal", semconv.TermaSkillOriginKey: semconv.TermaSkillOriginPersonal,
			semconv.TermaSkillDescriptionKey: "Cuts filler from prose.",
		})},
		{"1cc63765-abef-4bc9-97c3-85e0b1a9e714", with(map[string]any{
			semconv.TermaSkillNameKey: "antigravity-guide", semconv.TermaSkillOriginKey: semconv.TermaSkillOriginBuiltin,
			semconv.TermaSkillDescriptionKey: "Answers questions about agy.",
		})},
	}
	defs := hookruntest.Named(events, semconv.TermaSkillDefinitionEvent)
	if len(defs) != len(want) {
		t.Fatalf("spooled %d definitions, want %d: %+v", len(defs), len(want), defs)
	}
	for i, w := range want {
		got := defs[i].Attrs
		if defs[i].SessionID != w.session || len(got) != len(w.attrs) {
			t.Errorf("definition %d: %s %+v, want %s %+v", i, defs[i].SessionID, got, w.session, w.attrs)
			continue
		}
		for k, v := range w.attrs {
			if got[k] != v {
				t.Errorf("definition %d: %s = %v, want %v", i, k, got[k], v)
			}
		}
	}

	// A second pass defines nothing again, and still names each use.
	replay(t, env, payloads)
	again := hookruntest.Spooled(t, env.Spool)
	if defs := hookruntest.Named(again, semconv.TermaSkillDefinitionEvent); len(defs) != 0 {
		t.Errorf("defined again: %+v", defs)
	}
	checkCalls(t, again, capturedUses)
}

// While the policy withholds prompts or tool content no skill is defined or named.
func TestPostToolUseReportsNoSkillWhileThePolicyWithholds(t *testing.T) {
	for _, prompts := range []bool{false, true} {
		env, payloads := skillCapture(t)
		env.Policy.IncludePrompts, env.Policy.IncludeToolContent = prompts, !prompts
		replay(t, env, payloads)
		events := hookruntest.Spooled(t, env.Spool)
		if defs := hookruntest.Named(events, semconv.TermaSkillDefinitionEvent); len(defs) != 0 {
			t.Errorf("prompts %v: defined %+v", prompts, defs)
		}
		checkCalls(t, events, make([]string, len(capturedUses)))
	}
}

// A step loads a skill only through a read_file of a SKILL.md that succeeded, a relative
// Path read from the working directory.
func TestAntigravitySkillRead(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	abs := filepath.Join(t.TempDir(), "skills", "x", "SKILL.md")
	for _, tc := range []struct {
		name, payload, want string
	}{
		{"absolute", `{"conversationId":"c1","error":"","toolCall":{"name":"read_file","args":{"Path":"` + hookruntest.InJSON(abs) + `","StartLine":1,"EndLine":50}}}`, abs},
		{"relative", `{"conversationId":"c1","toolCall":{"name":"read_file","args":{"Path":".agents/skills/./x/SKILL.md"}}}`, filepath.Join(cwd, ".agents", "skills", "x", "SKILL.md")},
		{"failed", `{"conversationId":"c1","error":"file does not exist","toolCall":{"name":"read_file","args":{"Path":"` + hookruntest.InJSON(abs) + `"}}}`, ""},
		{"other-file", `{"conversationId":"c1","toolCall":{"name":"read_file","args":{"Path":"` + hookruntest.InJSON(filepath.Join(filepath.Dir(abs), "notes.md")) + `"}}}`, ""},
		{"view-file", `{"conversationId":"c1","toolCall":{"name":"view_file","args":{"AbsolutePath":"` + hookruntest.InJSON(abs) + `"}}}`, ""},
		{"dot-dot", `{"conversationId":"c1","toolCall":{"name":"read_file","args":{"Path":"` + hookruntest.InJSON(filepath.Join(filepath.Dir(abs), "link")) + `/../SKILL.md"}}}`, ""},
		{"lowercase-key", `{"conversationId":"c1","toolCall":{"name":"read_file","args":{"path":"` + hookruntest.InJSON(abs) + `"}}}`, ""},
		{"no-args", `{"conversationId":"c1","toolCall":{"name":"read_file"}}`, ""},
		{"no-call", `{"conversationId":"c1"}`, ""},
	} {
		in, err := readAntigravityInput(strings.NewReader(tc.payload))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := antigravitySkillRead(in, cwd); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A SKILL.md is agy's skill only exactly one folder below a skills folder of one of its
// roots, or of a plugin's there, and in a checkout only in a folder from the working directory
// up to its root; a plugin's skill is named plugin:name, never twice.
func TestAntigravitySkillByRoot(t *testing.T) {
	t.Parallel()
	repo := hookruntest.InitRepo(t)
	dotfiles := hookruntest.InitRepo(t)
	roots := antigravitySkillRoots{Config: t.TempDir(), Builtin: t.TempDir()}
	write := func(root, rel, frontmatter string) string {
		hookruntest.WriteFile(t, root, rel, "---\n"+frontmatter+"---\nbody\n")
		return filepath.Join(root, filepath.FromSlash(rel))
	}
	env := hookrun.Env{Policy: hookruntest.Admitting(repo)}
	env.Policy.Repositories = append(env.Policy.Repositories, "github.com/acme/"+filepath.Base(dotfiles))
	r := &hookrun.Repo{Root: repo}
	repoURL := "https://github.com/acme/" + filepath.Base(repo)

	// A personal skill whose folder links into a collected checkout links to that checkout.
	write(dotfiles, "skills/dotfile/SKILL.md", "name: dotfile\n")
	linked := filepath.Join(roots.Config, "skills", "linked")
	symlinked := true
	if err := os.MkdirAll(filepath.Dir(linked), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dotfiles, "skills", "dotfile"), linked); err != nil {
		symlinked = false
	}

	deep := hookrun.Skill{Name: "deep", Origin: semconv.TermaSkillOriginRepository, RepositoryURL: repoURL, Path: "sub/_agent/skills/deep/SKILL.md"}
	deepPath := write(repo, "sub/_agent/skills/deep/SKILL.md", "name: deep\n")
	for _, tc := range []struct {
		name, cwd, path string
		want            hookrun.Skill
		ok              bool
	}{
		{"two-deep", repo, write(repo, ".agents/skills/a/b/SKILL.md", "name: b\n"), hookrun.Skill{}, false},
		{"claude-skills", repo, write(repo, ".claude/skills/x/SKILL.md", "name: x\n"), hookrun.Skill{}, false},
		{"no-name", repo, write(roots.Config, "skills/folder/SKILL.md", "description: Named by its folder.\n"),
			hookrun.Skill{Name: "folder", Description: "Named by its folder.", Origin: semconv.TermaSkillOriginPersonal}, true},
		// agy lists a folder's skills only to a session working in it or below it.
		{"sub-folder-from-the-root", repo, deepPath, hookrun.Skill{}, false},
		{"sub-folder-from-a-sibling", filepath.Join(repo, "subway"), deepPath, hookrun.Skill{}, false},
		{"sub-folder-from-outside", t.TempDir(), deepPath, hookrun.Skill{}, false},
		{"sub-folder-from-itself", filepath.Join(repo, "sub"), deepPath, deep, true},
		{"sub-folder-from-below", filepath.Join(repo, "sub", "pkg", "x"), deepPath, deep, true},
		{"root-from-below", filepath.Join(repo, "sub"), write(repo, ".agents/skills/top/SKILL.md", "name: top\n"),
			hookrun.Skill{Name: "top", Origin: semconv.TermaSkillOriginRepository, RepositoryURL: repoURL, Path: ".agents/skills/top/SKILL.md"}, true},
		{"plugin-prefixed", repo, write(repo, ".agent/plugins/kit/skills/go/SKILL.md", "name: kit:go\n"),
			hookrun.Skill{Name: "kit:go", Origin: semconv.TermaSkillOriginPlugin, Plugin: "kit"}, true},
		{"plugin-named-as-a-skill-folder", repo, write(repo, ".agents/plugins/.agents/skills/go/SKILL.md", "name: go\n"),
			hookrun.Skill{Name: ".agents:go", Origin: semconv.TermaSkillOriginPlugin, Plugin: ".agents"}, true},
		{"plugin-personal", repo, write(roots.Config, "plugins/kit/skills/run/SKILL.md", "name: run\n"),
			hookrun.Skill{Name: "kit:run", Origin: semconv.TermaSkillOriginPlugin, Plugin: "kit"}, true},
		{"plugin-builtin", repo, write(roots.Builtin, "plugins/core/skills/help/SKILL.md", "name: help\n"),
			hookrun.Skill{Name: "core:help", Origin: semconv.TermaSkillOriginPlugin, Plugin: "core"}, true},
		{"builtin-reference", repo, write(roots.Builtin, "skills/guide/references/SKILL.md", "name: ref\n"), hookrun.Skill{}, false},
		{"elsewhere", repo, write(t.TempDir(), "skills/x/SKILL.md", "name: x\n"), hookrun.Skill{}, false},
		{"missing", repo, filepath.Join(roots.Config, "skills", "gone", "SKILL.md"), hookrun.Skill{}, false},
	} {
		env.Cwd = tc.cwd
		got, ok := roots.skill(env, r, tc.path)
		if ok != tc.ok || ok && got != tc.want {
			t.Errorf("%s: %+v, %v; want %+v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	// A skill agy ships carries no link, though its folder lies in a collected checkout.
	shipped := antigravitySkillRoots{Config: t.TempDir(), Builtin: filepath.Join(dotfiles, "builtin")}
	want := hookrun.Skill{Name: "guide", Origin: semconv.TermaSkillOriginBuiltin}
	if got, ok := shipped.skill(env, r, write(dotfiles, "builtin/skills/guide/SKILL.md", "name: guide\n")); !ok || got != want {
		t.Errorf("shipped: %+v, %v; want %+v", got, ok, want)
	}
	if symlinked {
		want := hookrun.Skill{Name: "dotfile", Origin: semconv.TermaSkillOriginPersonal,
			RepositoryURL: "https://github.com/acme/" + filepath.Base(dotfiles), Path: "skills/dotfile/SKILL.md"}
		if got, ok := roots.skill(env, r, filepath.Join(linked, "SKILL.md")); !ok || got != want {
			t.Errorf("linked: %+v, %v; want %+v", got, ok, want)
		}
	}
}
