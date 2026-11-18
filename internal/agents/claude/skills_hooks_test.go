package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// skillBody is in every skill's body in these tests: it must never leave.
const skillBody = "SKILL-BODY-MUST-NOT-LEAVE"

// skillSession is a Claude Code session in a collected repository whose transcript is the
// fixture, with each skill it loads written where it loaded from.
func skillSession(t *testing.T) (hookrun.Env, string) {
	t.Helper()
	env := newFundingEnv(t)
	claudeDir := os.Getenv("CLAUDE_CONFIG_DIR")
	tmp := t.TempDir()
	plugins := filepath.Join(claudeDir, "plugins")
	hookruntest.WriteFile(t, plugins, "known_marketplaces.json", `{"acme-tools":{"source":{"source":"github","repo":"acme/agent-plugins"}}}`)
	hookruntest.WriteFile(t, plugins, "marketplaces/acme-tools/.claude-plugin/marketplace.json", `{"plugins":[{"name":"tools","source":"./plugins/tools"}]}`)
	skill := func(name, frontmatter string) string {
		return "---\nname: " + name + "\n" + frontmatter + "---\n\n" + skillBody + "\n"
	}
	release := skill("release-notes", "description: Drafts release notes from merged pull requests.\n")
	hookruntest.WriteFile(t, plugins, "marketplaces/acme-tools/plugins/tools/skills/release-notes/SKILL.md", release)
	hookruntest.WriteFile(t, plugins, "cache/acme-tools/tools/1.2.0/skills/release-notes/SKILL.md", release)
	hookruntest.WriteFile(t, env.Cwd, ".claude/skills/hello-skill/SKILL.md", skill("hello-skill", "description: From the file, not the listing.\n"))
	hookruntest.WriteFile(t, env.Cwd, ".claude/skills/capped/SKILL.md", skill("capped", "description: |\n  "+strings.Repeat("A description longer than the listing allows. ", 40)+"\n"))
	hookruntest.WriteFile(t, env.Cwd, ".claude/skills/late/SKILL.md", skill("late", "description: Late.\n"))
	hookruntest.WriteFile(t, claudeDir, "skills/deploy/SKILL.md", skill("deploy", "description: Ships the build.\nwhen_to_use: Before a release.\n"))
	return env, skillFixture(t, env.Cwd, claudeDir, tmp)
}

// Each skill the session used leaves once, with where it is held and nothing of its body. A
// skill with a file takes its description from that file, whatever the listing says; one with
// none (dataviz, unpacked with no SKILL.md, and claude-api, with no directory) is reported
// without one. /compact and the failed call are no skills.
func TestStopSpoolsEachSkillTheSessionUsedOnce(t *testing.T) {
	env, path := skillSession(t)
	stopClaude(t, env, path)
	stopClaude(t, env, path)
	events := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSkillDefinitionEvent)
	repoURL := "https://github.com/acme/" + filepath.Base(env.Cwd)
	capped := strings.TrimSpace(strings.Repeat("A description longer than the listing allows. ", 40))
	want := []map[string]any{
		{semconv.TermaSkillNameKey: "tools:release-notes", semconv.TermaSkillDescriptionKey: "Drafts release notes from merged pull requests.",
			semconv.TermaSkillOriginKey: "plugin", semconv.TermaSkillPluginNameKey: "tools", semconv.TermaSkillMarketplaceNameKey: "acme-tools",
			semconv.TermaSkillRepositoryURLKey: "https://github.com/acme/agent-plugins", semconv.TermaSkillPathKey: "plugins/tools/skills/release-notes/SKILL.md"},
		{semconv.TermaSkillNameKey: "hello-skill", semconv.TermaSkillDescriptionKey: "From the file, not the listing.",
			semconv.TermaSkillOriginKey: "repository", semconv.TermaSkillRepositoryURLKey: repoURL, semconv.TermaSkillPathKey: ".claude/skills/hello-skill/SKILL.md"},
		{semconv.TermaSkillNameKey: "dataviz", semconv.TermaSkillOriginKey: "builtin"},
		{semconv.TermaSkillNameKey: "claude-api"},
		{semconv.TermaSkillNameKey: "deploy", semconv.TermaSkillDescriptionKey: "Ships the build. - Before a release.", semconv.TermaSkillOriginKey: "personal"},
		{semconv.TermaSkillNameKey: "capped", semconv.TermaSkillDescriptionKey: capped,
			semconv.TermaSkillOriginKey: "repository", semconv.TermaSkillRepositoryURLKey: repoURL, semconv.TermaSkillPathKey: ".claude/skills/capped/SKILL.md"},
		{semconv.TermaSkillNameKey: "late", semconv.TermaSkillDescriptionKey: "Late.",
			semconv.TermaSkillOriginKey: "repository", semconv.TermaSkillRepositoryURLKey: repoURL, semconv.TermaSkillPathKey: ".claude/skills/late/SKILL.md"},
	}
	if len(events) != len(want) {
		t.Fatalf("spooled %d definitions, want %d: %+v", len(events), len(want), events)
	}
	for i, ev := range events {
		for _, k := range []string{semconv.TermaSkillNameKey, semconv.TermaSkillDescriptionKey, semconv.TermaSkillOriginKey, semconv.TermaSkillPluginNameKey,
			semconv.TermaSkillMarketplaceNameKey, semconv.TermaSkillRepositoryURLKey, semconv.TermaSkillPathKey} {
			if ev.Attrs[k] != want[i][k] {
				t.Errorf("%v: %s = %q, want %q", want[i][semconv.TermaSkillNameKey], k, ev.Attrs[k], want[i][k])
			}
		}
		if ev.SessionID != titleSession || ev.Attrs[semconv.GenAIMainAgentNameKey] != claudeTool || ev.Attrs[semconv.TermaEvidenceSourceKey] != sourceClaudeTranscript {
			t.Errorf("%v: %+v", want[i][semconv.TermaSkillNameKey], ev)
		}
		if strings.Contains(fmt.Sprint(ev.Attrs), skillBody) {
			t.Errorf("a skill's body left: %+v", ev.Attrs)
		}
	}
}

// A policy withholding either kind of content sends no definition.
func TestStopSpoolsNoSkillWhileThePolicyWithholds(t *testing.T) {
	for _, prompts := range []bool{false, true} {
		env, path := skillSession(t)
		env.Policy.IncludePrompts, env.Policy.IncludeToolContent = prompts, !prompts
		stopClaude(t, env, path)
		if events := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSkillDefinitionEvent); len(events) != 0 {
			t.Errorf("prompts %v: spooled %+v", prompts, events)
		}
	}
}

// A session that moved into a collected checkout reports only what it used there: a skill
// it used where the team does not collect stays on the machine. A listing written there still
// says which names are skills, since a name leaves only through a use: a built-in it names and
// the session used in the collected checkout is reported, by its name alone.
func TestStopReportsNoSkillUsedWhereTheTeamDoesNotCollect(t *testing.T) {
	env := newFundingEnv(t)
	other := hookruntest.InitRepo(t) // a repository the policy does not list
	hookruntest.WriteFile(t, other, ".claude/skills/secret/SKILL.md", "---\nname: secret\ndescription: From the other checkout.\n---\n")
	hookruntest.WriteFile(t, env.Cwd, ".claude/skills/shared/SKILL.md", "---\nname: shared\ndescription: From the file.\n---\n")
	call := func(cwd, id, skill string) []string {
		dir := filepath.Join(cwd, ".claude", "skills", skill)
		return []string{
			`{"type":"assistant","cwd":` + jsonString(cwd) + `,"message":{"content":[{"type":"tool_use","id":"` + id + `","name":"Skill","input":{"skill":"` + skill + `"}}]}}`,
			`{"type":"user","isMeta":true,"cwd":` + jsonString(cwd) + `,"sourceToolUseID":"` + id + `","message":{"content":[{"type":"text","text":` + jsonString(claudeSkillBaseDir+dir+"\n\n"+skillBody) + `}]}}`,
		}
	}
	lines := []string{`{"type":"attachment","cwd":` + jsonString(other) + `,"attachment":{"type":"skill_listing","content":"- secret: Private to the other checkout.\n- shared: Listed in the other checkout.\n- built-in: Listed in the other checkout.","names":["secret","shared","built-in"]}}`}
	lines = append(lines, call(other, "toolu_1", "secret")...)
	lines = append(lines, call(env.Cwd, "toolu_2", "shared")...)
	lines = append(lines, `{"type":"assistant","cwd":`+jsonString(env.Cwd)+`,"message":{"content":[{"type":"tool_use","id":"toolu_3","name":"Skill","input":{"skill":"built-in"}}]}}`)
	stopClaude(t, env, transcript(t, t.TempDir(), lines...))
	events := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSkillDefinitionEvent)
	if len(events) != 2 || events[0].Attrs[semconv.TermaSkillNameKey] != "shared" || events[0].Attrs[semconv.TermaSkillDescriptionKey] != "From the file." ||
		events[0].Attrs[semconv.TermaSkillOriginKey] != semconv.TermaSkillOriginRepository {
		t.Fatalf("spooled %+v", events)
	}
	if b := events[1].Attrs; b[semconv.TermaSkillNameKey] != "built-in" || b[semconv.TermaSkillDescriptionKey] != nil || b[semconv.TermaSkillOriginKey] != nil {
		t.Errorf("the built-in: %+v", b)
	}
	if strings.Contains(fmt.Sprint(events), "other checkout") {
		t.Errorf("what the session did where the team does not collect left: %+v", events)
	}
}

// A skill used in a collected checkout takes its description from its own file, whatever a
// listing written in another checkout, since dropped by the policy, said of it; and a skill
// with no directory that only that listing named is still reported, by its name alone.
func TestStopDescribesASkillFromItsFileWhateverAListingElsewhereSaid(t *testing.T) {
	env := newFundingEnv(t)
	first, second := env.Cwd, hookruntest.InitRepo(t)
	hookruntest.WriteFile(t, second, ".claude/skills/shared/SKILL.md", "---\nname: shared\ndescription: From the second checkout's file.\n---\n")
	path := transcript(t, t.TempDir(), `{"type":"attachment","cwd":`+jsonString(first)+
		`,"attachment":{"type":"skill_listing","content":"- shared: Listed in the first checkout.\n- built-in: Listed in the first checkout.","names":["shared","built-in"]}}`)
	stopClaude(t, env, path) // saves the listing's offset

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cwd, dir := jsonString(second), filepath.Join(second, ".claude", "skills", "shared")
	_, err = f.WriteString(`{"type":"assistant","cwd":` + cwd + `,"message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Skill","input":{"skill":"shared"}}]}}` + "\n" +
		`{"type":"user","isMeta":true,"cwd":` + cwd + `,"sourceToolUseID":"toolu_1","message":{"content":[{"type":"text","text":` + jsonString(claudeSkillBaseDir+dir+"\n\n"+skillBody) + `}]}}` + "\n" +
		`{"type":"assistant","cwd":` + cwd + `,"message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Skill","input":{"skill":"built-in"}}]}}` + "\n" +
		`{"type":"user","isMeta":true,"cwd":` + cwd + `,"sourceToolUseID":"toolu_2","message":{"content":[{"type":"text","text":` + jsonString("# Built-in\n\n"+skillBody) + `}]}}` + "\n")
	if err != nil || f.Close() != nil {
		t.Fatal(err)
	}
	env.Cwd, env.Policy = second, hookruntest.Admitting(second) // the policy drops the first
	stopClaude(t, env, path)
	events := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSkillDefinitionEvent)
	if len(events) != 2 || events[0].Attrs[semconv.TermaSkillNameKey] != "shared" || events[0].Attrs[semconv.TermaSkillDescriptionKey] != "From the second checkout's file." ||
		events[1].Attrs[semconv.TermaSkillNameKey] != "built-in" || events[1].Attrs[semconv.TermaSkillDescriptionKey] != nil || events[1].Attrs[semconv.TermaSkillOriginKey] != nil {
		t.Fatalf("spooled %+v", events)
	}
	if s := fmt.Sprint(events); strings.Contains(s, "first checkout") || strings.Contains(s, skillBody) {
		t.Errorf("a listing's description or a body left: %+v", events)
	}
}

// A description leaves only when it is read from the frontmatter of the skill's own file.
// Every listing entry here carries a canary: a file with no description, no frontmatter, an
// unclosed one or none that can be read, a command and a built-in with no directory are all
// reported with no description, and no canary leaves.
func TestStopSendsADescriptionOnlyFromTheSkillsOwnFile(t *testing.T) {
	const canary = "LISTING-CANARY"
	env := newFundingEnv(t)
	cwd := jsonString(env.Cwd)
	skillDir := func(n string) string { return filepath.Join(env.Cwd, ".claude", "skills", n) }
	for n, file := range map[string]string{
		"qa-file":     "---\nname: qa-file\ndescription: From the file.\n---\n\n" + skillBody + "\n",
		"qa-nodesc":   "---\nname: qa-nodesc\n---\n\n" + skillBody + "\n",
		"qa-nofm":     skillBody + " with no frontmatter.\n",
		"qa-unclosed": "---\nname: qa-unclosed\ndescription: " + canary + " in a block never closed.\n\n" + skillBody + "\n",
	} {
		hookruntest.WriteFile(t, env.Cwd, ".claude/skills/"+n+"/SKILL.md", file)
	}
	// A SKILL.md that is a directory cannot be read as a file.
	if err := os.MkdirAll(filepath.Join(skillDir("qa-unreadable"), "SKILL.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	withDir := []string{"qa-file", "qa-nodesc", "qa-nofm", "qa-unclosed", "qa-unreadable"}
	names := append(slices.Clone(withDir), "qa-cmd", "qa-builtin")
	var listing []string
	for _, n := range names {
		listing = append(listing, "- "+n+": "+canary+" for "+n+".")
	}
	lines := []string{`{"type":"attachment","cwd":` + cwd + `,"attachment":{"type":"skill_listing","content":` + jsonString(strings.Join(listing, "\n")) +
		`,"names":["` + strings.Join(names, `","`) + `"]}}`}
	call := func(id, n, text string) []string {
		return []string{
			`{"type":"assistant","cwd":` + cwd + `,"message":{"content":[{"type":"tool_use","id":"` + id + `","name":"Skill","input":{"skill":"` + n + `"}}]}}`,
			`{"type":"user","isMeta":true,"cwd":` + cwd + `,"sourceToolUseID":"` + id + `","message":{"content":[{"type":"text","text":` + jsonString(text) + `}]}}`,
		}
	}
	for i, n := range withDir {
		lines = append(lines, call(fmt.Sprintf("toolu_%d", i), n, claudeSkillBaseDir+skillDir(n)+"\n\n"+skillBody)...)
	}
	lines = append(lines,
		`{"type":"user","cwd":`+cwd+`,"message":{"content":"<command-message>qa-cmd</command-message>\n<command-name>/qa-cmd</command-name>"}}`,
		`{"type":"user","isMeta":true,"cwd":`+cwd+`,"message":{"content":[{"type":"text","text":`+jsonString(skillBody+" the command's text.\n")+`}]}}`)
	lines = append(lines, call("toolu_9", "qa-builtin", "# Built-in\n\n"+skillBody)...)
	stopClaude(t, env, transcript(t, t.TempDir(), lines...))

	events := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSkillDefinitionEvent)
	got := map[string]any{}
	for _, ev := range events {
		got[ev.Attrs[semconv.TermaSkillNameKey].(string)] = ev.Attrs[semconv.TermaSkillDescriptionKey]
		if s := fmt.Sprint(ev.Attrs); strings.Contains(s, canary) || strings.Contains(s, skillBody) {
			t.Errorf("a listing's description or a body left: %+v", ev.Attrs)
		}
	}
	if len(got) != len(names) {
		t.Fatalf("spooled %d definitions, want %d: %+v", len(got), len(names), events)
	}
	for _, n := range names {
		var want any
		if n == "qa-file" {
			want = "From the file."
		}
		if got[n] != want {
			t.Errorf("%s: description %v, want %v", n, got[n], want)
		}
	}
}

// With GIT_DIR set every directory reads as that one checkout, so a record written anywhere
// but where the hook runs is not counted.
func TestStopReportsOnlyTheHooksOwnDirectoryUnderGitDir(t *testing.T) {
	env := newFundingEnv(t)
	other := t.TempDir()
	t.Setenv("GIT_DIR", filepath.Join(env.Cwd, ".git"))
	call := func(cwd, id, skill string) []string {
		hookruntest.WriteFile(t, cwd, ".claude/skills/"+skill+"/SKILL.md", "---\nname: "+skill+"\ndescription: Of "+skill+".\n---\n")
		dir := filepath.Join(cwd, ".claude", "skills", skill)
		return []string{
			`{"type":"assistant","cwd":` + jsonString(cwd) + `,"message":{"content":[{"type":"tool_use","id":"` + id + `","name":"Skill","input":{"skill":"` + skill + `"}}]}}`,
			`{"type":"user","isMeta":true,"cwd":` + jsonString(cwd) + `,"sourceToolUseID":"` + id + `","message":{"content":[{"type":"text","text":` + jsonString(claudeSkillBaseDir+dir+"\n\n"+skillBody) + `}]}}`,
		}
	}
	stopClaude(t, env, transcript(t, t.TempDir(), append(call(other, "toolu_1", "elsewhere"), call(env.Cwd, "toolu_2", "here")...)...))
	events := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSkillDefinitionEvent)
	if len(events) != 1 || events[0].Attrs[semconv.TermaSkillNameKey] != "here" {
		t.Fatalf("spooled %+v", events)
	}
}

// A Skill call Claude Code refused (an unknown skill) used none, though a listing names it.
func TestStopReportsNoSkillWhoseCallFailed(t *testing.T) {
	env := newFundingEnv(t)
	cwd := jsonString(env.Cwd)
	stopClaude(t, env, transcript(t, t.TempDir(),
		`{"type":"attachment","cwd":`+cwd+`,"attachment":{"type":"skill_listing","content":"- gone: Listed once.","names":["gone"]}}`,
		`{"type":"assistant","cwd":`+cwd+`,"message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Skill","input":{"skill":"gone"}}]}}`,
		`{"type":"user","cwd":`+cwd+`,"message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":"<tool_use_error>Unknown skill: gone</tool_use_error>"}]}}`))
	if events := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSkillDefinitionEvent); len(events) != 0 {
		t.Errorf("spooled %+v", events)
	}
}
