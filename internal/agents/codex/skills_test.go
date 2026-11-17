package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// A shell command reads a skill when a reader Codex knows prints a SKILL.md it names
// literally; a search, a listing or an expanded operand reads none.
func TestShellSkillReads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		command string
		want    []string
	}{
		{"cat a b/SKILL.md", []string{"/w/b/SKILL.md"}},
		{"sed -n '1,250p' x/SKILL.md; wc -l x/SKILL.md", []string{"/w/x/SKILL.md"}},
		{"cat x/SKILL.md && head -n 20 x/SKILL.md", []string{"/w/x/SKILL.md"}},
		{"cat /abs/skills/y/SKILL.md", []string{"/abs/skills/y/SKILL.md"}},
		{"cd /repo && nl -ba .agents/skills/z/SKILL.md", []string{"/repo/.agents/skills/z/SKILL.md"}},
		{"(cd /elsewhere) && cat z/SKILL.md", []string{"/w/z/SKILL.md"}},
		{"cd \"$DIR\" && cat z/SKILL.md", nil},
		{"command cat ./skills/../skills/q/SKILL.md", []string{"/w/skills/q/SKILL.md"}},
		{"cat x/SKILL.md.bak x/NOTSKILL.md", nil},
		{"sed -i 's/old/new/' x/SKILL.md", nil},
		{"sed -Ei.bak 's/old/new/' x/SKILL.md", nil},
		{"sed --in-place=.bak 's/old/new/' x/SKILL.md", nil},
		{"sed -I '' 's/old/new/' x/SKILL.md", nil},
		{"sed 's/old/new/' -i x/SKILL.md", nil},
		{"sed -ne/skill/p x/SKILL.md", []string{"/w/x/SKILL.md"}},
		{"sed -n -e'/description:/p' x/SKILL.md", []string{"/w/x/SKILL.md"}},
		{"sed -n -e -i x/SKILL.md", []string{"/w/x/SKILL.md"}},
		{"sed -n 1,20p -- x/SKILL.md", []string{"/w/x/SKILL.md"}},
		{"sed -l -i '' 's/old/new/' x/SKILL.md", nil},
		{"sed --expression s/old/new/ -i x/SKILL.md", nil},
		{"sed -n --expression -i x/SKILL.md", []string{"/w/x/SKILL.md"}},
		{"rg --files -g SKILL.md", nil},
		{"find . -name SKILL.md", nil},
		{"rg -n foo x/SKILL.md", nil},
		{"ls x/SKILL.md", nil},
		{`cat "$f"`, nil},
		{"cat $(find . -name SKILL.md)", nil},
		{"f() { cat x/SKILL.md; }", nil},
		{"cat 'unterminated", nil},
	} {
		if got := shellSkillReads(tc.command, "/w"); !slices.Equal(got, tc.want) {
			t.Errorf("%q: %v, want %v", tc.command, got, tc.want)
		}
	}
}

// A SKILL.md is a skill by the root it was read under, judged before its links resolve;
// a .claude/skills file is not one Codex uses.
func TestCodexSkillByRoot(t *testing.T) {
	t.Parallel()
	repo := hookruntest.InitRepo(t)
	roots := codexSkillRoots{Home: t.TempDir(), User: t.TempDir(), Managed: t.TempDir()}
	write := func(root, rel, frontmatter string) string {
		hookruntest.WriteFile(t, root, rel, "---\n"+frontmatter+"---\nbody\n")
		return filepath.Join(root, filepath.FromSlash(rel))
	}
	env := hookrun.Env{Policy: hookruntest.Admitting(repo)}
	r := &hookrun.Repo{Root: repo}
	repoURL := "https://github.com/acme/" + filepath.Base(repo)
	for _, tc := range []struct {
		name, path string
		want       hookrun.Skill
		ok         bool
	}{
		{"system", write(roots.Home, "skills/.system/imagegen/SKILL.md", "name: imagegen\ndescription: Makes images.\n"),
			hookrun.Skill{Name: "imagegen", Description: "Makes images.", Origin: semconv.TermaSkillOriginBuiltin}, true},
		{"codex-home", write(roots.Home, "skills/mine/SKILL.md", "description: Folder   named.\n"),
			hookrun.Skill{Name: "mine", Description: "Folder named.", Origin: semconv.TermaSkillOriginPersonal}, true},
		{"agents-home", write(roots.User, ".agents/skills/home/SKILL.md", "name: Home  Skill\ndescription: x\n"),
			hookrun.Skill{Name: "Home Skill", Description: "x", Origin: semconv.TermaSkillOriginPersonal}, true},
		{"plugin", write(roots.Home, "plugins/cache/acme/kit/1.0.0/skills/go/SKILL.md", "name: go\ndescription: Go.\n"),
			hookrun.Skill{Name: "kit:go", Description: "Go.", Origin: semconv.TermaSkillOriginPlugin, Plugin: "kit", Marketplace: "acme"}, true},
		{"managed", write(roots.Managed, "policy/SKILL.md", "name: policy\n"),
			hookrun.Skill{Name: "policy", Origin: semconv.TermaSkillOriginManaged}, true},
		{"repository-agents", write(repo, ".agents/skills/hello/SKILL.md", "name: hello\ndescription: Build for AWS: ECS\n"),
			hookrun.Skill{Name: "hello", Description: "Build for AWS: ECS", Origin: semconv.TermaSkillOriginRepository, RepositoryURL: repoURL, Path: ".agents/skills/hello/SKILL.md"}, true},
		{"repository-codex", write(repo, "sub/.codex/skills/deep/SKILL.md", "name: deep\n"),
			hookrun.Skill{Name: "deep", Origin: semconv.TermaSkillOriginRepository, RepositoryURL: repoURL, Path: "sub/.codex/skills/deep/SKILL.md"}, true},
		{"claude-skills", write(repo, ".claude/skills/c/SKILL.md", "name: c\n"), hookrun.Skill{}, false},
		{"no-frontmatter", func() string {
			hookruntest.WriteFile(t, roots.Home, "skills/plain/SKILL.md", "# Plain\n")
			return filepath.Join(roots.Home, "skills", "plain", "SKILL.md")
		}(), hookrun.Skill{}, false},
		{"elsewhere", write(t.TempDir(), "skills/x/SKILL.md", "name: x\n"), hookrun.Skill{}, false},
		{"missing", filepath.Join(roots.Home, "skills", "gone", "SKILL.md"), hookrun.Skill{}, false},
	} {
		got, ok := roots.skill(env, r, tc.path)
		if ok != tc.ok || ok && got != tc.want {
			t.Errorf("%s: %+v, %v; want %+v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// codexSkillEnv is a Codex hook in a collected repository, with private Codex and home
// folders, whose developer did not choose Codex at setup.
func codexSkillEnv(t *testing.T) hookrun.Env {
	t.Helper()
	env := fundingEnv(t)
	t.Setenv("HOME", t.TempDir())
	return env
}

func readSkill(t *testing.T, env hookrun.Env, command string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"session_id": testCodexID, "cwd": env.Cwd, "tool_name": codexShellTool, "tool_use_id": "call_1",
		"tool_input": map[string]any{"command": command}})
	env.Stdin = strings.NewReader(string(b))
	if err := postToolUse(context.Background(), env); err != nil {
		t.Fatal(err)
	}
}

// A shell call that prints a skill reports it once per session, from each root Codex reads
// skills from, never from .claude/skills, and not while the policy withholds content.
func TestPostToolUseReportsTheSkillsAShellCallRead(t *testing.T) {
	env := codexSkillEnv(t)
	home := os.Getenv("CODEX_HOME")
	hookruntest.WriteFile(t, home, "skills/mine/SKILL.md", "---\nname: mine\ndescription: Personal.\n---\n")
	hookruntest.WriteFile(t, home, "skills/.system/sys/SKILL.md", "---\nname: sys\ndescription: Built in.\n---\n")
	hookruntest.WriteFile(t, env.Cwd, ".agents/skills/hello-codex/SKILL.md", "---\nname: hello-codex\ndescription: Repository.\n---\n")
	hookruntest.WriteFile(t, env.Cwd, ".claude/skills/other/SKILL.md", "---\nname: other\n---\n")
	readSkill(t, env, "sed -n '1,200p' "+filepath.Join(home, "skills", "mine", "SKILL.md"))
	readSkill(t, env, "cat .agents/skills/hello-codex/SKILL.md .claude/skills/other/SKILL.md")
	readSkill(t, env, "cat "+filepath.Join(home, "skills", ".system", "sys", "SKILL.md"))
	readSkill(t, env, "head -n 5 .agents/skills/hello-codex/SKILL.md")
	events := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSkillDefinitionEvent)
	var got []string
	for _, ev := range events {
		got = append(got, ev.Attrs[semconv.TermaSkillNameKey].(string)+"/"+ev.Attrs[semconv.TermaSkillOriginKey].(string))
		if ev.SessionID != testCodexID || ev.Attrs[semconv.TermaEvidenceSourceKey] != sourceCodexHook || ev.Attrs[semconv.GenAIMainAgentNameKey] != codexTool {
			t.Errorf("%+v", ev)
		}
	}
	if strings.Join(got, " ") != "mine/personal hello-codex/repository sys/builtin" {
		t.Errorf("spooled %v", got)
	}
	if len(events) == 3 && events[1].Attrs[semconv.TermaSkillPathKey] != ".agents/skills/hello-codex/SKILL.md" {
		t.Errorf("repository skill: %+v", events[1].Attrs)
	}

	withheld := codexSkillEnv(t)
	withheld.Policy.IncludeToolContent = false
	hookruntest.WriteFile(t, withheld.Cwd, ".agents/skills/hello-codex/SKILL.md", "---\nname: hello-codex\n---\n")
	readSkill(t, withheld, "cat .agents/skills/hello-codex/SKILL.md")
	if events := hookruntest.Named(hookruntest.Spooled(t, withheld.Spool), semconv.TermaSkillDefinitionEvent); len(events) != 0 {
		t.Errorf("spooled under a withholding policy: %+v", events)
	}
}

// skillRollout writes the recorded rollout under CODEX_HOME's sessions: its turns run in
// turnDir, its skill in the session's repository.
func skillRollout(t *testing.T, env hookrun.Env, turnDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "codex_rollout_skill.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(data), `"cwd":"{{REPO}}"`, `"cwd":"`+hookruntest.InJSON(turnDir)+`"`)
	text = strings.NewReplacer("{{ID}}", testCodexID, "{{REPO}}", hookruntest.InJSON(env.Cwd), "{{CODEX_HOME}}", hookruntest.InJSON(os.Getenv("CODEX_HOME"))).Replace(text)
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "11", "17", "rollout-2026-11-17T14-48-36-"+testCodexID+".jsonl")
	hookruntest.WriteFile(t, filepath.Dir(path), filepath.Base(path), text)
	hookruntest.WriteFile(t, env.Cwd, ".agents/skills/hello-codex/SKILL.md",
		"---\nname: hello-codex\ndescription: Replies with a fixed greeting. Use when the user asks for the hello-codex greeting.\n---\n\nReply with exactly: HELLO-FROM-CODEX-SKILL\n")
	return path
}

func stopSkillSession(t *testing.T, env hookrun.Env, path string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"session_id": testCodexID, "cwd": env.Cwd, "transcript_path": path})
	env.Stdin = strings.NewReader(string(b))
	if err := stop(context.Background(), env); err != nil {
		t.Fatal(err)
	}
}

// A $name skill the rollout records is reported once, whether or not the developer chose
// Codex at setup, and only from a turn the team collects.
func TestStopReportsTheRolloutsNamedSkills(t *testing.T) {
	env := codexSkillEnv(t)
	path := skillRollout(t, env, env.Cwd)
	stopSkillSession(t, env, path)
	stopSkillSession(t, env, path)
	events := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSkillDefinitionEvent)
	if len(events) != 1 {
		t.Fatalf("spooled %d definitions, want 1: %+v", len(events), events)
	}
	want := map[string]any{
		semconv.TermaSkillNameKey: "hello-codex", semconv.TermaSkillOriginKey: semconv.TermaSkillOriginRepository,
		semconv.TermaSkillDescriptionKey:   "Replies with a fixed greeting. Use when the user asks for the hello-codex greeting.",
		semconv.TermaSkillRepositoryURLKey: "https://github.com/acme/" + filepath.Base(env.Cwd), semconv.TermaSkillPathKey: ".agents/skills/hello-codex/SKILL.md",
		semconv.TermaEvidenceSourceKey: sourceCodexRollout, semconv.GenAIMainAgentNameKey: codexTool,
	}
	for k, v := range want {
		if events[0].Attrs[k] != v {
			t.Errorf("%s = %v, want %v", k, events[0].Attrs[k], v)
		}
	}

	elsewhere := codexSkillEnv(t)
	other := hookruntest.InitRepo(t) // a repository the policy does not list
	stopSkillSession(t, elsewhere, skillRollout(t, elsewhere, other))
	if events := hookruntest.Named(hookruntest.Spooled(t, elsewhere.Spool), semconv.TermaSkillDefinitionEvent); len(events) != 0 {
		t.Errorf("a turn the team does not collect reported %+v", events)
	}
}

// The rollout's capture reads nothing while the policy withholds either kind of content.
func TestStopReportsNoNamedSkillWhileThePolicyWithholds(t *testing.T) {
	env := codexSkillEnv(t)
	env.Policy = config.Policy{Mode: config.ModeRepo, Repositories: env.Policy.Repositories, IncludeToolContent: true}
	stopSkillSession(t, env, skillRollout(t, env, env.Cwd))
	if events := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSkillDefinitionEvent); len(events) != 0 {
		t.Errorf("spooled %+v", events)
	}
}
