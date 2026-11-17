package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// skillFixture is the recorded transcript with its placeholders filled: the session's
// repository, Claude Code's config directory and the temporary folder bundled skills unpack to.
func skillFixture(t *testing.T, repo, claudeDir, tmp string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "claude_transcript_skills.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.NewReplacer("{{ID}}", titleSession, "{{REPO}}", hookruntest.InJSON(repo),
		"{{CLAUDE}}", hookruntest.InJSON(claudeDir), "{{TMP}}", hookruntest.InJSON(tmp)).Replace(string(data))
	dir := t.TempDir()
	hookruntest.WriteFile(t, dir, titleSession+".jsonl", text)
	return filepath.Join(dir, titleSession+".jsonl")
}

// A listing's entries are found by name, so a description may span lines or look like an
// entry itself; one the listing dropped or cut is not whole.
func TestParseSkillListing(t *testing.T) {
	t.Parallel()
	content := "- deploy\n" +
		"- multi: First line: with a colon.\n- not-a-name: yes\nthird line.\n" +
		"- capped: Too long to list whole…\n" +
		"- shown (Shown Name): With a display name.\n" +
		"- deploy-extra: Its name begins with another's."
	got := parseSkillListing(content, []string{"deploy", "multi", "capped", "shown", "deploy-extra", "missing"})
	want := map[string]claudeListed{
		"deploy":       {},
		"multi":        {Description: "First line: with a colon.\n- not-a-name: yes\nthird line."},
		"capped":       {Description: "Too long to list whole…", Shortened: true},
		"shown":        {Description: "With a display name."},
		"deploy-extra": {Description: "Its name begins with another's."},
	}
	if len(got) != len(want) {
		t.Errorf("parsed %d entries, want %d: %+v", len(got), len(want), got)
	}
	for n, w := range want {
		if got[n] != w {
			t.Errorf("%s = %+v, want %+v", n, got[n], w)
		}
	}
}

// A description holding a line that reads as the next skill's entry leaves both entries'
// edges unsure, so neither description counts as whole; a name that spans lines has no entry.
func TestParseSkillListingDoubtsAnEntryItCannotBound(t *testing.T) {
	t.Parallel()
	got := parseSkillListing("- a: First line.\n- b: looks like b's entry\n- b: The real one.\n- c: Clear.", []string{"a", "b", "c"})
	if got["a"].whole() || got["b"].whole() || !got["c"].whole() || got["c"].Description != "Clear." {
		t.Errorf("entries = %+v", got)
	}
	got = parseSkillListing("- x\n- y: Y.", []string{"x\n- y", "y"})
	if _, ok := got["x\n- y"]; ok || got["y"].Description != "Y." {
		t.Errorf("entries = %+v", got)
	}
}

// A cursor past its bound of listings keeps the session's first, whole one.
func TestReadClaudeSkillUsesKeepsTheFirstListing(t *testing.T) {
	t.Parallel()
	const listing = `{"type":"attachment","attachment":{"type":"skill_listing","content":"- a: A.","names":["a"]}}`
	lines := make([]string, claudeSkillListingsKept+3)
	for i := range lines {
		lines[i] = listing
	}
	_, cur, err := readClaudeSkillUses(transcript(t, t.TempDir(), lines...), claudeSkillCursor{})
	last := int64((len(lines) - 1) * (len(listing) + 1))
	if err != nil || len(cur.Listings) != claudeSkillListingsKept || cur.Listings[0] != 0 || cur.Listings[len(cur.Listings)-1] != last {
		t.Errorf("listings = %v, %v", cur.Listings, err)
	}
}

// A later listing replaces an entry when it holds the whole description, never with a
// dropped or cut one.
func TestReadClaudeSkillListingsMergesInOrder(t *testing.T) {
	t.Parallel()
	listing := func(content string, names ...string) string {
		return `{"type":"attachment","attachment":{"type":"skill_listing","content":` + jsonString(content) + `,"names":["` + strings.Join(names, `","`) + `"]}}`
	}
	lines := []string{
		listing("- a: First.\n- b: Whole.\n- c", "a", "b", "c"),
		listing("- a: Second.\n- b: Cut…\n- c: Now listed.", "a", "b", "c"),
		`{"type":"user","message":{"content":"not a listing"}}`,
	}
	path := transcript(t, t.TempDir(), lines...)
	offsets := []int64{0, int64(len(lines[0]) + 1), int64(len(lines[0]) + len(lines[1]) + 2)}
	got := readClaudeSkillListings(path, offsets)
	for n, w := range map[string]string{"a": "Second.", "b": "Whole.", "c": "Now listed."} {
		if got[n].Description != w {
			t.Errorf("%s = %q, want %q", n, got[n].Description, w)
		}
	}
}

// The fixture's uses: a person's /plugin:skill, the model's calls with the directory each
// loaded from, a built-in with none, /compact and a failed call, and a skill listed later.
func TestReadClaudeSkillUsesOnTheFixture(t *testing.T) {
	t.Parallel()
	repo, claudeDir, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	path := skillFixture(t, repo, claudeDir, tmp)
	uses, cur, err := readClaudeSkillUses(path, claudeSkillCursor{})
	if err != nil {
		t.Fatal(err)
	}
	want := []claudeSkillUse{
		{Name: "tools:release-notes", Dir: filepath.Join(claudeDir, "plugins", "cache", "acme-tools", "tools", "1.2.0", "skills", "release-notes"), slash: true},
		{Name: "hello-skill", Dir: filepath.Join(repo, ".claude", "skills", "hello-skill"), toolUseID: "toolu_01HELLO"},
		{Name: "compact", slash: true},
		{Name: "dataviz", Dir: filepath.Join(tmp, "claude-1000", "bundled-skills", "2.1.326", "2175d6973433b385bb475e160eb15a01", "dataviz"), toolUseID: "toolu_02DATAVIZ"},
		{Name: "claude-api", toolUseID: "toolu_03API"},
		{Name: "deploy", Dir: filepath.Join(claudeDir, "skills", "deploy"), toolUseID: "toolu_04DEPLOY"},
		{Name: "capped", Dir: filepath.Join(repo, ".claude", "skills", "capped"), toolUseID: "toolu_05CAPPED"},
		{Name: "hello-skill", Dir: filepath.Join(repo, ".claude", "skills", "hello-skill"), toolUseID: "toolu_06AGAIN"},
		{Name: "no-such-skill", toolUseID: "toolu_07NOPE"},
		{Name: "late", Dir: filepath.Join(repo, ".claude", "skills", "late"), slash: true},
	}
	if len(uses) != len(want) {
		t.Fatalf("read %d uses, want %d: %+v", len(uses), len(want), uses)
	}
	for i := range want {
		if filepath.FromSlash(uses[i].Dir) != want[i].Dir || uses[i].Name != want[i].Name || uses[i].toolUseID != want[i].toolUseID || uses[i].slash != want[i].slash {
			t.Errorf("use %d = %+v, want %+v", i, uses[i], want[i])
		}
	}
	if info, _ := os.Stat(path); cur.Offset != info.Size() || len(cur.Listings) != 2 {
		t.Errorf("cursor = %+v, want the file's end and its two listings", cur)
	}
	listed := readClaudeSkillListings(path, cur.Listings)
	if !listed["late"].whole() || listed["deploy"].Description != "" || !listed["capped"].Shortened || !strings.Contains(listed["claude-api"].Description, "\nSKIP only") {
		t.Errorf("listings = %+v", listed)
	}
	if again, _, err := readClaudeSkillUses(path, cur); err != nil || len(again) != 0 {
		t.Errorf("a second read from the cursor gave %+v, %v", again, err)
	}
}

// A skill's origin comes from where it loaded, and only a file a collected checkout or a
// plugin's GitHub marketplace holds says where it is held.
func TestClaudeSkillSource(t *testing.T) {
	t.Parallel()
	session, shared, private := hookruntest.InitRepo(t), hookruntest.InitRepo(t), hookruntest.InitRepo(t)
	roots := claudeSkillRoots{Config: t.TempDir(), Managed: t.TempDir()}
	plugins := filepath.Join(roots.Config, "plugins")
	hookruntest.WriteFile(t, plugins, "known_marketplaces.json",
		`{"acme-tools":{"source":{"source":"github","repo":"acme/agent-plugins"}},"local":{"source":{"source":"directory","path":"/src/plugins"}}}`)
	hookruntest.WriteFile(t, plugins, "marketplaces/acme-tools/.claude-plugin/marketplace.json",
		`{"name":"acme-tools","plugins":[{"name":"tools","source":"./plugins/tools"},{"name":"remote","source":{"source":"github","repo":"x/y"}}]}`)
	hookruntest.WriteFile(t, plugins, "marketplaces/acme-tools/plugins/tools/skills/release-notes/SKILL.md", "---\nname: release-notes\n---\n")
	for _, root := range []string{shared, private, session} {
		hookruntest.WriteFile(t, root, ".claude/skills/kept/SKILL.md", "---\nname: kept\n---\n")
	}
	personal := filepath.Join(roots.Config, "skills")
	if err := os.MkdirAll(personal, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"shared": shared, "private": private} {
		if err := os.Symlink(filepath.Join(target, ".claude", "skills", "kept"), filepath.Join(personal, name)); err != nil {
			t.Skipf("symlinks: %v", err)
		}
	}
	env := hookrun.Env{Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/" + filepath.Base(session), "github.com/acme/" + filepath.Base(shared)}}}
	r := &hookrun.Repo{Root: session}
	for _, tc := range []struct {
		name, dir string
		want      hookrun.Skill
	}{
		{"plugin-github", filepath.Join(plugins, "cache", "acme-tools", "tools", "1.2.0", "skills", "release-notes"), hookrun.Skill{
			Origin: semconv.TermaSkillOriginPlugin, Plugin: "tools", Marketplace: "acme-tools",
			RepositoryURL: "https://github.com/acme/agent-plugins", Path: "plugins/tools/skills/release-notes/SKILL.md"}},
		{"plugin-not-in-checkout", filepath.Join(plugins, "cache", "acme-tools", "tools", "1.2.0", "skills", "other"), hookrun.Skill{
			Origin: semconv.TermaSkillOriginPlugin, Plugin: "tools", Marketplace: "acme-tools"}},
		{"plugin-object-source", filepath.Join(plugins, "cache", "acme-tools", "remote", "2.0.0", "skills", "x"), hookrun.Skill{
			Origin: semconv.TermaSkillOriginPlugin, Plugin: "remote", Marketplace: "acme-tools"}},
		{"plugin-local-marketplace", filepath.Join(plugins, "cache", "local", "kit", "0.1.0", "skills", "y"), hookrun.Skill{
			Origin: semconv.TermaSkillOriginPlugin, Plugin: "kit", Marketplace: "local"}},
		{"personal-in-admitted", filepath.Join(personal, "shared"), hookrun.Skill{
			Origin: semconv.TermaSkillOriginPersonal, RepositoryURL: "https://github.com/acme/" + filepath.Base(shared), Path: ".claude/skills/kept/SKILL.md"}},
		{"personal-in-private", filepath.Join(personal, "private"), hookrun.Skill{Origin: semconv.TermaSkillOriginPersonal}},
		{"repository", filepath.Join(session, ".claude", "skills", "kept"), hookrun.Skill{
			Origin: semconv.TermaSkillOriginRepository, RepositoryURL: "https://github.com/acme/" + filepath.Base(session), Path: ".claude/skills/kept/SKILL.md"}},
		{"bundled", filepath.Join(t.TempDir(), "claude-1000", "bundled-skills", "2.1.326", "abc", "dataviz"), hookrun.Skill{Origin: semconv.TermaSkillOriginBuiltin}},
		{"managed", filepath.Join(roots.Managed, ".claude", "skills", "policy"), hookrun.Skill{Origin: semconv.TermaSkillOriginManaged}},
		{"repository-elsewhere", filepath.Join(session, "docs", "skills", "kept"), hookrun.Skill{}},
		{"unknown", filepath.Join(t.TempDir(), "skills", "x"), hookrun.Skill{}},
		{"relative", "skills/x", hookrun.Skill{}},
	} {
		if got := roots.source(env, r, tc.dir); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
