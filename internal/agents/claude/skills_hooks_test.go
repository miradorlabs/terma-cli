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

// Each skill the session used leaves once, with its description and where it is held, and
// nothing of its body: deploy and capped take theirs from the file, as the listing dropped or
// cut it; /compact and the failed call are no skills.
func TestStopSpoolsEachSkillTheSessionUsedOnce(t *testing.T) {
	env, path := skillSession(t)
	stopClaude(t, env, path)
	stopClaude(t, env, path)
	events := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSkillDefinitionEvent)
	repoURL := "https://github.com/acme/" + filepath.Base(env.Cwd)
	capped := strings.TrimSpace(strings.Repeat("A description longer than the listing allows. ", 40))
	want := []map[string]any{
		{semconv.TermaSkillNameKey: "tools:release-notes", semconv.TermaSkillDescriptionKey: "Drafts release notes from merged pull requests. Use before cutting a release.",
			semconv.TermaSkillOriginKey: "plugin", semconv.TermaSkillPluginNameKey: "tools", semconv.TermaSkillMarketplaceNameKey: "acme-tools",
			semconv.TermaSkillRepositoryURLKey: "https://github.com/acme/agent-plugins", semconv.TermaSkillPathKey: "plugins/tools/skills/release-notes/SKILL.md"},
		{semconv.TermaSkillNameKey: "hello-skill", semconv.TermaSkillDescriptionKey: `Replies with a fixed greeting. Use when the user says "hello skill" or invokes /hello-skill.`,
			semconv.TermaSkillOriginKey: "repository", semconv.TermaSkillRepositoryURLKey: repoURL, semconv.TermaSkillPathKey: ".claude/skills/hello-skill/SKILL.md"},
		{semconv.TermaSkillNameKey: "dataviz", semconv.TermaSkillDescriptionKey: "Use this skill whenever you are about to create a chart.", semconv.TermaSkillOriginKey: "builtin"},
		{semconv.TermaSkillNameKey: "claude-api", semconv.TermaSkillDescriptionKey: "Reference for the Claude API.\nTRIGGER — read first, before you answer.\nSKIP only when another provider is being worked on."},
		{semconv.TermaSkillNameKey: "deploy", semconv.TermaSkillDescriptionKey: "Ships the build. - Before a release.", semconv.TermaSkillOriginKey: "personal"},
		{semconv.TermaSkillNameKey: "capped", semconv.TermaSkillDescriptionKey: capped,
			semconv.TermaSkillOriginKey: "repository", semconv.TermaSkillRepositoryURLKey: repoURL, semconv.TermaSkillPathKey: ".claude/skills/capped/SKILL.md"},
		{semconv.TermaSkillNameKey: "late", semconv.TermaSkillDescriptionKey: "Added to the session after it started.",
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
