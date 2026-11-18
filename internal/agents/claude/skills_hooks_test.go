package claude

import (
	"fmt"
	"os"
	"path/filepath"
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
// none (dataviz, claude-api) takes the listing's. /compact and the failed call are no skills.
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
		{semconv.TermaSkillNameKey: "dataviz", semconv.TermaSkillDescriptionKey: "Use this skill whenever you are about to create a chart.", semconv.TermaSkillOriginKey: "builtin"},
		{semconv.TermaSkillNameKey: "claude-api", semconv.TermaSkillDescriptionKey: "Reference for the Claude API.\nTRIGGER — read first, before you answer.\nSKIP only when another provider is being worked on."},
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
// it used, and the listing it was given, where the team does not collect stay on the machine.
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
	lines := []string{`{"type":"attachment","cwd":` + jsonString(other) + `,"attachment":{"type":"skill_listing","content":"- secret: Private to the other checkout.\n- shared: Listed in the other checkout.","names":["secret","shared"]}}`}
	lines = append(lines, call(other, "toolu_1", "secret")...)
	lines = append(lines, call(env.Cwd, "toolu_2", "shared")...)
	stopClaude(t, env, transcript(t, t.TempDir(), lines...))
	events := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSkillDefinitionEvent)
	if len(events) != 1 || events[0].Attrs[semconv.TermaSkillNameKey] != "shared" || events[0].Attrs[semconv.TermaSkillDescriptionKey] != "From the file." ||
		events[0].Attrs[semconv.TermaSkillOriginKey] != semconv.TermaSkillOriginRepository {
		t.Fatalf("spooled %+v", events)
	}
	if strings.Contains(fmt.Sprint(events), "other checkout") {
		t.Errorf("what the session did where the team does not collect left: %+v", events)
	}
}

// A listing saved while the team collected its checkout is not read again once the policy
// drops that checkout: a same-named skill used in another collected checkout takes its
// description from its own file.
func TestStopLeavesOutAListingSavedWhereTheTeamNoLongerCollects(t *testing.T) {
	env := newFundingEnv(t)
	first, second := env.Cwd, hookruntest.InitRepo(t)
	hookruntest.WriteFile(t, second, ".claude/skills/shared/SKILL.md", "---\nname: shared\ndescription: From the second checkout's file.\n---\n")
	path := transcript(t, t.TempDir(), `{"type":"attachment","cwd":`+jsonString(first)+`,"attachment":{"type":"skill_listing","content":"- shared: Listed in the first checkout.","names":["shared"]}}`)
	stopClaude(t, env, path) // saves the listing's offset: the first checkout is collected

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(second, ".claude", "skills", "shared")
	_, err = f.WriteString(`{"type":"assistant","cwd":` + jsonString(second) + `,"message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Skill","input":{"skill":"shared"}}]}}` + "\n" +
		`{"type":"user","isMeta":true,"cwd":` + jsonString(second) + `,"sourceToolUseID":"toolu_1","message":{"content":[{"type":"text","text":` + jsonString(claudeSkillBaseDir+dir+"\n\n"+skillBody) + `}]}}` + "\n")
	if err != nil || f.Close() != nil {
		t.Fatal(err)
	}
	env.Cwd, env.Policy = second, hookruntest.Admitting(second) // the policy drops the first
	stopClaude(t, env, path)
	events := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSkillDefinitionEvent)
	if len(events) != 1 || events[0].Attrs[semconv.TermaSkillNameKey] != "shared" || events[0].Attrs[semconv.TermaSkillDescriptionKey] != "From the second checkout's file." {
		t.Fatalf("spooled %+v", events)
	}
}

// Claude Code lists a skill or command with no description in its frontmatter by its body's
// first line: that never leaves. A skill's file decides where it is read; elsewhere the
// listing's description leaves only when the loaded text shows it is not the body's.
func TestStopSendsNoDescriptionMadeFromASkillsBody(t *testing.T) {
	env := newFundingEnv(t)
	cwd := jsonString(env.Cwd)
	long := "BODY-CANARY-LONG " + strings.Repeat("a long first line ", 10)
	files := map[string]string{
		"qa-nodesc":  "---\nname: qa-nodesc\n---\n\nBODY-CANARY-NODESC this first line.\n",
		"qa-heading": "---\nname: qa-heading\nwhen_to_use: Never.\n---\n\n# BODY-CANARY-HEADING\n",
		"qa-long":    "---\nname: qa-long\n---\n" + long + "\n",
		"qa-nofm":    "BODY-CANARY-NOFM no frontmatter at all.\n",
		"qa-stale":   "---\nname: qa-stale\ndescription: Open <name> & go.\n---\n\nBODY-CANARY-STALE the line listed before the file had a description.\n",
	}
	for n, body := range files {
		hookruntest.WriteFile(t, env.Cwd, ".claude/skills/"+n+"/SKILL.md", body)
	}
	bodies := map[string]string{
		"qa-nodesc":  "BODY-CANARY-NODESC this first line.\n",
		"qa-heading": "# BODY-CANARY-HEADING\n",
		"qa-long":    long + "\n",
		"qa-nofm":    "BODY-CANARY-NOFM no frontmatter at all.\n",
		"qa-stale":   "BODY-CANARY-STALE the line listed before the file had a description.\n",
	}
	cutLong, _ := claudeOpening(long)
	entries := []struct{ name, description string }{
		{"qa-nodesc", "BODY-CANARY-NODESC this first line."},
		{"qa-heading", "BODY-CANARY-HEADING - Never."},
		{"qa-long", cutLong + "..."},
		{"qa-stale", "BODY-CANARY-STALE the line listed before the file had a description."},
		{"qa-angle", "BODY-CANARY-ANGLE review &lt;target&gt; & report it"},
		{"qa-gap", "BODY-CANARY-GAP a command whose text is not the next record."},
		{"qa-headed", "Deploy the app to &lt;staging&gt;."},
		{"qa-nofm", "BODY-CANARY-NOFM no frontmatter at all."},
		{"qa-cmd", "BODY-CANARY-CMD the command's first line."},
		{"qa-args", "BODY-CANARY-ARGS run with $ARGUMENTS"},
		{"qa-real", "Runs the real thing."},
		{"qa-unread", "Listed, but its text was never read."},
	}
	var listing []string
	var names []string
	for _, en := range entries {
		listing = append(listing, "- "+en.name+": "+en.description)
		names = append(names, en.name)
	}
	lines := []string{`{"type":"attachment","cwd":` + cwd + `,"attachment":{"type":"skill_listing","content":` + jsonString(strings.Join(listing, "\n")) +
		`,"names":["` + strings.Join(names, `","`) + `"]}}`}
	for i, n := range []string{"qa-nodesc", "qa-heading", "qa-long", "qa-nofm", "qa-stale"} {
		id := fmt.Sprintf("toolu_%d", i)
		dir := filepath.Join(env.Cwd, ".claude", "skills", n)
		lines = append(lines,
			`{"type":"assistant","cwd":`+cwd+`,"message":{"content":[{"type":"tool_use","id":"`+id+`","name":"Skill","input":{"skill":"`+n+`"}}]}}`,
			`{"type":"user","isMeta":true,"cwd":`+cwd+`,"sourceToolUseID":"`+id+`","message":{"content":[{"type":"text","text":`+jsonString(claudeSkillBaseDir+dir+"\n\n"+bodies[n])+`}]}}`)
	}
	for n, text := range map[string]string{
		"qa-cmd":  "BODY-CANARY-CMD the command's first line.\nMore of it.\n",
		"qa-args": "BODY-CANARY-ARGS run with v2\n",
		"qa-real": "BODY-CANARY-REAL a body unlike its description.\n",
		// Claude Code lists angle brackets as entities.
		"qa-angle": "BODY-CANARY-ANGLE review <target> & report it\n",
		// A description that only starts with the body's heading is the author's.
		"qa-headed": "# Deploy\n\nBODY-CANARY-HEADED steps.\n",
	} {
		lines = append(lines,
			`{"type":"user","cwd":`+cwd+`,"message":{"content":"<command-message>`+n+`</command-message>\n<command-name>/`+n+`</command-name>"}}`,
			`{"type":"user","isMeta":true,"cwd":`+cwd+`,"message":{"content":[{"type":"text","text":`+jsonString(text)+`}]}}`)
	}
	// A later record that names no call (a pasted image) is not the command's text.
	lines = append(lines,
		`{"type":"user","cwd":`+cwd+`,"message":{"content":"<command-message>qa-gap</command-message>\n<command-name>/qa-gap</command-name>"}}`,
		`{"type":"system","cwd":`+cwd+`}`,
		`{"type":"user","isMeta":true,"cwd":`+cwd+`,"message":{"content":[{"type":"text","text":"[Image: source: /tmp/shot.png]"}]}}`)
	lines = append(lines, `{"type":"assistant","cwd":`+cwd+`,"message":{"content":[{"type":"tool_use","id":"toolu_9","name":"Skill","input":{"skill":"qa-unread"}}]}}`)
	stopClaude(t, env, transcript(t, t.TempDir(), lines...))

	events := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSkillDefinitionEvent)
	got := map[string]any{}
	for _, ev := range events {
		got[ev.Attrs[semconv.TermaSkillNameKey].(string)] = ev.Attrs[semconv.TermaSkillDescriptionKey]
		if strings.Contains(fmt.Sprint(ev.Attrs), "BODY-CANARY") {
			t.Errorf("a skill's body left: %+v", ev.Attrs)
		}
	}
	if len(got) != len(entries) {
		t.Fatalf("spooled %d definitions, want %d: %+v", len(got), len(entries), events)
	}
	sent := map[string]any{"qa-real": "Runs the real thing.", "qa-stale": "Open <name> & go.", "qa-headed": "Deploy the app to <staging>."}
	for _, en := range entries {
		want := sent[en.name]
		if got[en.name] != want {
			t.Errorf("%s: description %q, want %v", en.name, got[en.name], want)
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
