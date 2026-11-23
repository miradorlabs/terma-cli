package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// skillEnv is a hook in a repository the policy collects, content on.
func skillEnv(t *testing.T) (Env, *Repo, *spool.Spool) {
	t.Helper()
	root, stateDir, sp := hookruntest.Project(t)
	env := Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Spool: sp, Team: hookruntest.Team, Policy: hookruntest.Admitting(root)}
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return env, r, sp
}

var skillBase = map[string]any{semconv.GenAIMainAgentNameKey: "agent", semconv.TermaEvidenceSourceKey: "agent_source"}

// A description is sent whole or not at all: kept at SkillDescriptionMaxChars characters,
// left out one past it or when it is not UTF-8, the definition still sent without it.
func TestSpoolSkillsSendsADescriptionOnlyWhole(t *testing.T) {
	t.Parallel()
	env, r, sp := skillEnv(t)
	atBound := strings.Repeat("é", SkillDescriptionMaxChars)
	skills := []Skill{
		{Name: "at-bound", Description: atBound},
		{Name: "past-bound", Description: atBound + "x"},
		{Name: "not-utf8", Description: "bad \xff byte"},
		{Name: "spaced", Description: "  trimmed \n"},
		{Name: "", Description: "no name"},
		{Name: "bad-\xff-name", Description: "invalid name"},
	}
	if !SpoolSkills(env, r, "skills", "sess-skill-1", skillBase, skills) {
		t.Fatal("SpoolSkills reported a failure")
	}
	events := hookruntest.Named(hookruntest.Spooled(t, sp), semconv.TermaSkillDefinitionEvent)
	want := map[string]any{"at-bound": atBound, "past-bound": nil, "not-utf8": nil, "spaced": "trimmed"}
	if len(events) != len(want) {
		t.Fatalf("spooled %d definitions, want %d: %+v", len(events), len(want), events)
	}
	for _, ev := range events {
		name, _ := ev.Attrs[semconv.TermaSkillNameKey].(string)
		if got := ev.Attrs[semconv.TermaSkillDescriptionKey]; got != want[name] {
			t.Errorf("%s: description = %.40q, want %.40q", name, got, want[name])
		}
		if ev.SessionID != "sess-skill-1" || ev.Attrs[semconv.GenAIMainAgentNameKey] != "agent" || ev.Attrs[AttrProjectID] != hookruntest.Team {
			t.Errorf("%s: carries %+v", name, ev)
		}
	}
}

// A skill's repository leaves only as an https URL with no credentials, and its file's path
// only with it, relative to it; an origin outside the registry's list is left out.
func TestSpoolSkillsSendsOnlyAShareableLink(t *testing.T) {
	t.Parallel()
	const goodURL, goodPath = "https://github.com/acme/widgets", ".agents/skills/x/SKILL.md"
	const plugin = semconv.TermaSkillOriginPlugin
	for _, tc := range []struct {
		name, origin, url, path string
		withURL, withPath       bool
	}{
		{"both", "", goodURL, goodPath, true, true},
		{"plugin-both", plugin, goodURL, goodPath, true, true},
		{"plugin-url-alone", plugin, goodURL, "", true, false},
		{"url-alone", "", goodURL, "", false, false},
		{"personal-url-alone", semconv.TermaSkillOriginPersonal, goodURL, "", false, false},
		{"path-alone", plugin, "", goodPath, false, false},
		{"absolute", "", goodURL, "/home/dev/.agents/skills/x/SKILL.md", false, false},
		{"home", "", goodURL, "~/.agents/skills/x/SKILL.md", false, false},
		{"dotdot", "", goodURL, "skills/../../etc/passwd", false, false},
		{"backslash", "", goodURL, `skills\x\SKILL.md`, false, false},
		{"drive", "", goodURL, "C:/skills/x/SKILL.md", false, false},
		{"plugin-absolute", plugin, goodURL, "/home/dev/plugins/x/SKILL.md", false, false},
		{"http", plugin, "http://github.com/acme/widgets", goodPath, false, false},
		{"userinfo", plugin, "https://token@github.com/acme/widgets", "", false, false},
		{"no-host", "", "https:///acme/widgets", goodPath, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := skillAttrs(Skill{Name: "x", Origin: tc.origin, RepositoryURL: tc.url, Path: tc.path})
			_, hasURL := out[semconv.TermaSkillRepositoryURLKey]
			_, hasPath := out[semconv.TermaSkillPathKey]
			if hasURL != tc.withURL || hasPath != tc.withPath {
				t.Errorf("origin %q url %q path %q: attrs %+v", tc.origin, tc.url, tc.path, out)
			}
		})
	}
	if out := skillAttrs(Skill{Name: "x", Origin: "elsewhere"}); out[semconv.TermaSkillOriginKey] != nil {
		t.Errorf("an unknown origin was sent: %+v", out)
	}
}

// A session reports each skill once, across calls and across two hooks at the same moment.
func TestSpoolSkillsReportsEachSkillOncePerSession(t *testing.T) {
	t.Parallel()
	env, r, sp := skillEnv(t)
	skill := []Skill{{Name: "deploy", Origin: semconv.TermaSkillOriginPersonal}}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if !SpoolSkills(env, r, "skills", "sess-skill-2", skillBase, skill) {
				t.Error("a concurrent call failed")
			}
		})
	}
	wg.Wait()
	SpoolSkills(env, r, "skills", "sess-skill-2", skillBase, append(skill, Skill{Name: "Deploy"}))
	SpoolSkills(env, r, "skills", "sess-skill-3", skillBase, skill) // another session's own
	events := hookruntest.Named(hookruntest.Spooled(t, sp), semconv.TermaSkillDefinitionEvent)
	var got []string
	for _, ev := range events {
		got = append(got, ev.SessionID+"/"+ev.Attrs[semconv.TermaSkillNameKey].(string))
	}
	if strings.Join(got, " ") != "sess-skill-2/deploy sess-skill-2/Deploy sess-skill-3/deploy" {
		t.Errorf("spooled %v", got)
	}
	if events[0].Attrs[semconv.TermaSkillOriginKey] != semconv.TermaSkillOriginPersonal {
		t.Errorf("origin = %v", events[0].Attrs[semconv.TermaSkillOriginKey])
	}
	state, _ := os.ReadFile(filepath.Join(env.StateDir, "skills", EvidenceID("sess-skill-2")+".json"))
	if strings.Contains(string(state), "deploy") {
		t.Errorf("the state names the skill: %s", state)
	}
}

// A policy that withholds prompts or tool content sends no definition, and no spool fails.
func TestSpoolSkillsSendsNothingWhileThePolicyWithholds(t *testing.T) {
	t.Parallel()
	for _, withhold := range []func(*config.Policy){
		func(p *config.Policy) { p.IncludePrompts = false },
		func(p *config.Policy) { p.IncludeToolContent = false },
		func(p *config.Policy) { p.CollectsNothing = true },
	} {
		env, r, sp := skillEnv(t)
		withhold(&env.Policy)
		if env.SkillsCollected() || !SpoolSkills(env, r, "skills", "sess-skill-4", skillBase, []Skill{{Name: "deploy"}}) {
			t.Error("a withholding policy collects skills, or failed the caller")
		}
		if events := hookruntest.Spooled(t, sp); len(events) != 0 {
			t.Errorf("spooled %+v", events)
		}
	}
	env, r, _ := skillEnv(t)
	env.Spool = nil
	if SpoolSkills(env, r, "skills", "sess-skill-4", skillBase, []Skill{{Name: "deploy"}}) {
		t.Error("a skill with no spool to go to reported success")
	}
}

// Signed out, a skill's use is lost and counted, and nothing is queued; the skill stays
// unreported, so its next use in the session, under a sign-in, is sent once, in that
// sign-in's lane.
func TestSpoolSkillsWithNoLaneLosesTheUseAndSendsTheNextOne(t *testing.T) {
	t.Parallel()
	p := newPushRepo(t)
	skill := []Skill{{Name: "deploy"}}
	out, outDir := lanedHook(t, p, "", false, false)
	r, err := out.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !SpoolSkills(out, r, "skills", "sess-skill-5", skillBase, skill) {
		t.Fatal("a lost use failed the caller")
	}
	if evs := hookruntest.Spooled(t, out.Spool); len(evs) != 0 {
		t.Fatalf("signed out, queued %s", hookruntest.Names(evs))
	}
	if l, err := spool.ReadLosses(outDir, time.Now()); err != nil || l.Counts[sendas.LostSignedOut] != 1 {
		t.Fatalf("losses = %v, %v; want the use counted once", l.Counts, err)
	}
	in, _ := lanedHook(t, p, "aaaaaaaaaaaaaaaa", false, false)
	for range 2 {
		if !SpoolSkills(in, r, "skills", "sess-skill-5", skillBase, skill) {
			t.Fatal("SpoolSkills reported a failure")
		}
	}
	evs := hookruntest.Named(hookruntest.Spooled(t, in.Spool), semconv.TermaSkillDefinitionEvent)
	if len(evs) != 1 || evs[0].Account != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("queued %+v; want one definition in the sign-in's lane", evs)
	}
}

// SkillLink names the checkout a skill's file resolves into, and only one the policy collects.
func TestSkillLinkNamesOnlyACollectedCheckout(t *testing.T) {
	t.Parallel()
	session := hookruntest.InitRepo(t)
	shared := hookruntest.InitRepo(t)
	private := hookruntest.InitRepo(t)
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	for _, root := range []string{session, shared, private, outside} {
		hookruntest.WriteFile(t, root, "skills/x/SKILL.md", "---\nname: x\n---\n")
	}
	home, _ := filepath.EvalSymlinks(t.TempDir())
	for name, target := range map[string]string{"shared": shared, "private": private, "outside": outside} {
		if err := os.Symlink(filepath.Join(target, "skills", "x"), filepath.Join(home, name)); err != nil {
			t.Skipf("symlinks: %v", err)
		}
	}
	env := Env{Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/" + filepath.Base(session), "github.com/acme/" + filepath.Base(shared)}}}
	for _, tc := range []struct {
		name, file, url, path string
	}{
		{"in-session", filepath.Join(session, "skills", "x", "SKILL.md"), "https://github.com/acme/" + filepath.Base(session), "skills/x/SKILL.md"},
		{"linked-admitted", filepath.Join(home, "shared", "SKILL.md"), "https://github.com/acme/" + filepath.Base(shared), "skills/x/SKILL.md"},
		{"linked-not-admitted", filepath.Join(home, "private", "SKILL.md"), "", ""},
		{"outside-git", filepath.Join(home, "outside", "SKILL.md"), "", ""},
		{"missing", filepath.Join(session, "skills", "y", "SKILL.md"), "", ""},
		{"directory", filepath.Join(session, "skills", "x"), "", ""},
	} {
		u, p := env.SkillLink(tc.file)
		if u != tc.url || p != tc.path || filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
			t.Errorf("%s: SkillLink = %q, %q; want %q, %q", tc.name, u, p, tc.url, tc.path)
		}
	}
}

// A skill's frontmatter is read as YAML alone: its name, description and when_to_use, each
// only where YAML gives a string. A block YAML refuses, or one the read cuts before it
// closes, gives nothing, so no text from another key's value, a comment or the body is taken
// for a description.
func TestReadSkillFrontmatter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A block that runs on, as valid YAML, to a "---" exactly where the read stops: closed
	// when the file ends there, cut when it goes on ("---example").
	head := "---\nname: x\ndescription: CUT-TEXT\n"
	pad := "#" + strings.Repeat("x", skillFileLimit-3-len(head)-2) + "\n"
	none := SkillFrontmatter{}
	for _, tc := range []struct {
		name, content string
		want          SkillFrontmatter
		ok            bool
	}{
		{"plain", "---\nname: deploy\ndescription: Ships the build.\n---\nbody\n", SkillFrontmatter{Name: "deploy", Description: "Ships the build."}, true},
		{"quoted", "---\nname: \"deploy\"\ndescription: 'Ships   the build.'\nwhen_to_use: \"Before a release.\"\n---\n", SkillFrontmatter{Name: "deploy", Description: "Ships the build.", WhenToUse: "Before a release."}, true},
		{"block", "---\nname: multi\ndescription: |\n  First line.\n  Second line.\nwhen_to_use: >\n  When testing.\n---\n", SkillFrontmatter{Name: "multi", Description: "First line. Second line.", WhenToUse: "When testing."}, true},
		{"inline-comment", "---\nname: x\ndescription: foo # note\n---\n", SkillFrontmatter{Name: "x", Description: "foo"}, true},
		{"comment-value", "---\nname: x\ndescription: # editor note\n---\n", SkillFrontmatter{Name: "x"}, true},
		{"not-a-string", "---\nname: x\ndescription: 42\nwhen_to_use: [a, b]\n---\n", SkillFrontmatter{Name: "x"}, true},
		{"hyphen-alias", "---\nname: x\ndescription: Ships builds.\nwhen-to-use: Friday\n---\n", SkillFrontmatter{Name: "x", Description: "Ships builds."}, true},
		{"quote-spans-lines", "---\nname: x\nargument-hint: \"first line of the hint\ndescription: TEXT-FROM-HINT\nend of hint\"\n---\n", SkillFrontmatter{Name: "x"}, true},
		{"crlf", "---\r\nname: win\r\ndescription: Windows lines.\r\n---\r\n", SkillFrontmatter{Name: "win", Description: "Windows lines."}, true},
		{"no-name", "---\ndescription: Folder named.\n---\n", SkillFrontmatter{Description: "Folder named."}, true},
		// Blocks YAML refuses give nothing, whatever a line of them looks like.
		{"unquoted-colon", "---\nname: aws\ndescription: Build for AWS: ECS and Lambda\n---\n", none, false},
		{"other-key-quote", "---\nname: x\nargument-hint: \"first line of the hint\ndescription: TEXT-FROM-HINT\nend of hint\"\nbad: a: b\n---\n", none, false},
		{"other-key-overwrite", "---\nname: x\ndescription: Real.\nargument-hint: 'first\ndescription: TEXT-FROM-HINT\nend'\nbad: a: b\n---\n", none, false},
		{"other-key-flow", "---\nname: x\ntags: [one,\ndescription: TEXT-FROM-FLOW\n]\nbad: a: b\n---\n", none, false},
		{"comment-refused", "---\nname: probe\ndescription: # editor note\nother: a: b\n---\n", none, false},
		{"inline-comment-refused", "---\nname: x\ndescription: foo # note\nbad: a: b\n---\n", none, false},
		{"duplicate-key", "---\nname: x\nname: y\ndescription: Real.\n---\n", none, false},
		// A mistyped close runs the block into the body, which YAML refuses: neither the
		// real description nor the body's is taken, as the reader cannot tell where the
		// author meant the frontmatter to end.
		{"mistyped-close", "---\nname: x\ndescription: Real.\n----\n\n# Body\n\nExample:\ndescription: BODY-TEXT\n---\nmore\n", none, false},
		// YAML ends a document at "...", so the body after it is not read.
		{"dots-close", "---\nname: x\ndescription: Real.\n...\n\n# Body\n\nExample:\ndescription: BODY-TEXT\n---\nmore\n", SkillFrontmatter{Name: "x", Description: "Real."}, true},
		{"none", "# Deploy\n\nname: deploy\n", none, false},
		{"unclosed", "---\nname: deploy\n", none, false},
		{"over-bound", "---\nname: big\ndescription: " + strings.Repeat("x", skillFileLimit) + "\n---\n", none, false},
		{"cut-at-bound", head + pad + "---example\nmore\n", none, false},
		{"ends-at-bound", head + pad + "---", SkillFrontmatter{Name: "x", Description: "CUT-TEXT"}, true},
		{"long-body", "---\nname: long\n---\n" + strings.Repeat("body\n", skillFileLimit), SkillFrontmatter{Name: "long"}, true},
	} {
		path := filepath.Join(dir, tc.name+".md")
		if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, ok := ReadSkillFrontmatter(path)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: %+v, %v; want %+v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	if n := len(head + pad + "---"); n != skillFileLimit {
		t.Fatalf("the bound's fixture is %d bytes, want %d", n, skillFileLimit)
	}
	if _, ok := ReadSkillFrontmatter(dir); ok {
		t.Error("a directory read as a skill")
	}
	if _, ok := ReadSkillFrontmatter(filepath.Join(dir, "missing.md")); ok {
		t.Error("a missing file read as a skill")
	}
}
