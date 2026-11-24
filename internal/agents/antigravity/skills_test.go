package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
// read, as they were, and the capture's payloads with its paths moved there, each
// conversation's transcript in place. Antigravity is the developer's choice.
func skillCapture(t *testing.T) (env hookrun.Env, payloads []string) {
	t.Helper()
	root := hookruntest.InitRepo(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
			writeTranscript(t, line)
		}
	}
	if len(payloads) != 10 {
		t.Fatalf("the capture has %d payloads, want 10", len(payloads))
	}
	return skillEnv(t, root), payloads
}

// skillEnv is a hook's environment in the collected checkout at root, with Antigravity chosen
// and a policy that collects prompts and tool content.
func skillEnv(t *testing.T, root string) hookrun.Env {
	t.Helper()
	sp, err := hookruntest.OpenSpool(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return hookrun.Env{StateDir: t.TempDir(), Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Agents: []string{"antigravity"}, Spool: sp, Version: "test"}
}

// writeTranscript makes the transcript a payload names, empty, unless it is there.
func writeTranscript(t *testing.T, payload string) {
	t.Helper()
	var in struct {
		TranscriptPath string `json:"transcriptPath"`
	}
	if err := json.Unmarshal([]byte(payload), &in); err != nil || in.TranscriptPath == "" {
		t.Fatalf("no transcript in %s: %v", payload, err)
	}
	if _, err := os.Stat(in.TranscriptPath); err == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(in.TranscriptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in.TranscriptPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// beginning is the PreInvocation that begins the turn of a conversation's first PostToolUse
// payload, as agy runs it: invocation 0, the step before the payload's, and the payload's
// conversation, folders, model and transcript.
func beginning(t *testing.T, payload string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		t.Fatal(err)
	}
	step, _ := m["stepIdx"].(float64)
	for _, k := range []string{"toolCall", "stepIdx", "error"} {
		delete(m, k)
	}
	m["hookEventName"], m["invocationNum"], m["initialNumSteps"] = "PreInvocation", 0, step-1
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// runHook runs h on payload as agy's hook would.
func runHook(t *testing.T, env hookrun.Env, h func(context.Context, hookrun.Env) error, payload string) {
	t.Helper()
	var stdout bytes.Buffer
	env.Stdin, env.Stdout = strings.NewReader(payload), &stdout
	if err := h(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout.String()) != "{}" {
		t.Fatalf("agy expects {} on stdout, got %q", stdout.String())
	}
}

// replay runs each payload through postToolUse as agy's hook would, each conversation's turn
// begun by a PreInvocation before its first.
func replay(t *testing.T, env hookrun.Env, payloads []string) {
	t.Helper()
	begun := map[string]bool{}
	for _, p := range payloads {
		var in struct {
			ID string `json:"conversationId"`
		}
		if err := json.Unmarshal([]byte(p), &in); err != nil {
			t.Fatal(err)
		}
		if !begun[in.ID] {
			begun[in.ID] = true
			runHook(t, env, preInvocation, beginning(t, p))
		}
		runHook(t, env, postToolUse, p)
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

// Each read that loaded a skill in an agy conversation names it on its terma.tool.call; a
// read that loaded no skill names none, and every step is still a call.
func TestPostToolUseNamesTheSkillsAgyLoaded(t *testing.T) {
	env, payloads := skillCapture(t)
	replay(t, env, payloads)
	checkCalls(t, hookruntest.Spooled(t, env.Spool), capturedUses)
}

// While the policy withholds prompts or tool content, or the developer did not choose
// Antigravity, no skill is named, and every step is still a call.
func TestPostToolUseNamesNoSkillWhileThePolicyWithholds(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		prompts, toolContent bool
		agents               []string
	}{
		{"prompts withheld", false, true, []string{"antigravity"}},
		{"tool content withheld", true, false, []string{"antigravity"}},
		{"both withheld", false, false, []string{"antigravity"}},
		{"antigravity not chosen", true, true, []string{"codex"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, payloads := skillCapture(t)
			env.Policy.IncludePrompts, env.Policy.IncludeToolContent, env.Agents = tc.prompts, tc.toolContent, tc.agents
			replay(t, env, payloads)
			checkCalls(t, hookruntest.Spooled(t, env.Spool), make([]string, len(capturedUses)))
		})
	}
}

// skillRead is a conversation's PostToolUse payload for a read_file of path at step 2, its
// transcript at transcript, working in cwd.
func skillRead(t *testing.T, id, transcript, cwd, path string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"conversationId": id, "cwd": cwd, "workspacePaths": []string{cwd}, "modelName": "gemini-3.8-flash-low",
		"hookEventName": "PostToolUse", "error": "", "stepIdx": 2, "transcriptPath": transcript,
		"toolCall": map[string]any{"name": "read_file", "args": map[string]any{"Path": path}},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, string(b))
	return string(b)
}

// agyTranscript is the transcript of conversation id in the Antigravity home at home, whose
// app data folder is appData.
func agyTranscript(home, appData, id string) string {
	return filepath.Join(home, appData, "brain", id, ".system_generated", "logs", antigravityTranscriptName)
}

// A personal skill whose folder links into another checkout is named only when the policy
// collects that checkout: the read's content is judged where the file is, and a name rides
// only where the read's arguments may go.
func TestPostToolUseNamesALinkedSkillOnlyFromACollectedCheckout(t *testing.T) {
	for _, collected := range []bool{false, true} {
		root := hookruntest.InitRepo(t)
		home, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		dotfiles := hookruntest.InitRepo(t)
		hookruntest.WriteFile(t, dotfiles, "skills/dotfile/SKILL.md", "---\nname: dotfile\n---\nbody\n")
		gemini := filepath.Join(home, ".gemini")
		if err := os.MkdirAll(filepath.Join(gemini, "config", "skills"), 0o700); err != nil {
			t.Fatal(err)
		}
		linked := filepath.Join(gemini, "config", "skills", "linked")
		if err := os.Symlink(filepath.Join(dotfiles, "skills", "dotfile"), linked); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
		env := skillEnv(t, root)
		if collected {
			env.Policy.Repositories = append(env.Policy.Repositories, hookruntest.Admitting(dotfiles).Repositories...)
		}
		id := "5b0c7f2e-1d4a-4c8e-9f31-0a6b2d9e7c11"
		p := skillRead(t, id, agyTranscript(gemini, "antigravity-cli", id), root, filepath.Join(linked, "SKILL.md"))
		replay(t, env, []string{p})
		want := ""
		if collected {
			want = "dotfile"
		}
		checkCalls(t, hookruntest.Spooled(t, env.Spool), []string{want})
	}
}

// The skill folders outside a checkout are the session's own Antigravity home's: a skill in a
// T3 Code thread's home is named for a session of that home, and is no skill to a session of
// another.
func TestPostToolUseNamesSkillsFromTheSessionsOwnHome(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t3 := filepath.Join(base, ".t3", "userdata", "providers", "antigravity", "acct")
	hookruntest.WriteFile(t, t3, "config/skills/x/SKILL.md", "---\nname: x\n---\nbody\n")
	skill := filepath.Join(t3, "config", "skills", "x", "SKILL.md")
	for _, tc := range []struct {
		name, home, appData, want string
	}{
		{"its own home", t3, t3AppDataDir, "x"},
		{"another home", filepath.Join(base, ".gemini"), "antigravity-cli", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := hookruntest.InitRepo(t)
			env := skillEnv(t, root)
			id := "9e4d1a6b-2c3f-4b5a-8d7e-6f0a1b2c3d4e"
			replay(t, env, []string{skillRead(t, id, agyTranscript(tc.home, tc.appData, id), root, skill)})
			checkCalls(t, hookruntest.Spooled(t, env.Spool), []string{tc.want})
		})
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
	builtin, config, dotfiles := t.TempDir(), t.TempDir(), t.TempDir()
	roots := []string{builtin, config}
	write := func(root, rel, frontmatter string) string {
		hookruntest.WriteFile(t, root, rel, "---\n"+frontmatter+"---\nbody\n")
		return filepath.Join(root, filepath.FromSlash(rel))
	}

	deepPath := write(repo, "sub/_agent/skills/deep/SKILL.md", "name: deep\n")
	for _, tc := range []struct {
		name, cwd, path string
		want            string
		ok              bool
	}{
		{"two-deep", repo, write(repo, ".agents/skills/a/b/SKILL.md", "name: b\n"), "", false},
		{"claude-skills", repo, write(repo, ".claude/skills/x/SKILL.md", "name: x\n"), "", false},
		{"no-name", repo, write(config, "skills/folder/SKILL.md", "description: Named by its folder.\n"), "folder", true},
		{"builtin", repo, write(builtin, "skills/guide_folder/SKILL.md", "name: guide\n"), "guide", true},
		// agy lists a folder's skills only to a session working in it or below it.
		{"sub-folder-from-the-root", repo, deepPath, "", false},
		{"sub-folder-from-a-sibling", filepath.Join(repo, "subway"), deepPath, "", false},
		{"sub-folder-from-outside", t.TempDir(), deepPath, "", false},
		{"sub-folder-from-itself", filepath.Join(repo, "sub"), deepPath, "deep", true},
		{"sub-folder-from-below", filepath.Join(repo, "sub", "pkg", "x"), deepPath, "deep", true},
		{"root-from-below", filepath.Join(repo, "sub"), write(repo, ".agents/skills/top/SKILL.md", "name: top\n"), "top", true},
		{"plugin-prefixed", repo, write(repo, ".agent/plugins/kit/skills/go/SKILL.md", "name: kit:go\n"), "kit:go", true},
		{"plugin-named-as-a-skill-folder", repo, write(repo, ".agents/plugins/.agents/skills/go/SKILL.md", "name: go\n"), ".agents:go", true},
		{"plugin-personal", repo, write(config, "plugins/kit/skills/run/SKILL.md", "name: run\n"), "kit:run", true},
		{"plugin-builtin", repo, write(builtin, "plugins/core/skills/help/SKILL.md", "name: help\n"), "core:help", true},
		{"builtin-reference", repo, write(builtin, "skills/guide/references/SKILL.md", "name: ref\n"), "", false},
		{"elsewhere", repo, write(t.TempDir(), "skills/x/SKILL.md", "name: x\n"), "", false},
		{"missing", repo, filepath.Join(config, "skills", "gone", "SKILL.md"), "", false},
	} {
		got, ok := antigravitySkillName(roots, repo, tc.cwd, tc.path)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	// A personal skill whose folder links into another checkout is judged where agy found
	// it, and named from the file the link leads to.
	write(dotfiles, "skills/dotfile/SKILL.md", "name: dotfile\n")
	linked := filepath.Join(config, "skills", "linked")
	if err := os.Symlink(filepath.Join(dotfiles, "skills", "dotfile"), linked); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if got, ok := antigravitySkillName(roots, repo, repo, filepath.Join(linked, "SKILL.md")); !ok || got != "dotfile" {
		t.Errorf("linked: %q, %v; want dotfile", got, ok)
	}
}

// A skill's frontmatter names it only where YAML accepts the block and gives the name as a
// string; a block YAML refuses, none, or one the read cuts before it closes reads as no skill.
func TestAntigravitySkillFrontmatterName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A block that runs on, as valid YAML, to a "---" exactly where the read stops: closed
	// when the file ends there, cut when it goes on ("---example").
	head := "---\nname: x\ndescription: CUT-TEXT\n"
	pad := "#" + strings.Repeat("x", skillFileLimit-3-len(head)-2) + "\n"
	for _, tc := range []struct {
		name, content string
		want          string
		ok            bool
	}{
		{"plain", "---\nname: deploy\ndescription: Ships the build.\n---\nbody\n", "deploy", true},
		{"quoted", "---\nname: \"deploy   it\"\ndescription: 'Ships   the build.'\n---\n", "deploy it", true},
		{"crlf", "---\r\nname: win\r\ndescription: Windows lines.\r\n---\r\n", "win", true},
		{"no-name", "---\ndescription: Folder named.\n---\n", "", true},
		{"not-a-string", "---\nname: 42\n---\n", "", true},
		// Blocks YAML refuses give nothing, whatever a line of them looks like.
		{"unquoted-colon", "---\nname: aws\ndescription: Build for AWS: ECS and Lambda\n---\n", "", false},
		{"duplicate-key", "---\nname: x\nname: y\ndescription: Real.\n---\n", "", false},
		// A mistyped close runs the block into the body, which YAML refuses.
		{"mistyped-close", "---\nname: x\ndescription: Real.\n----\n\n# Body\n\nExample:\nname: BODY-TEXT\n---\nmore\n", "", false},
		// YAML ends a document at "...", so the body after it is not read.
		{"dots-close", "---\nname: x\ndescription: Real.\n...\n\n# Body\n\nExample:\nname: BODY-TEXT\n---\nmore\n", "x", true},
		{"none", "# Deploy\n\nname: deploy\n", "", false},
		{"unclosed", "---\nname: deploy\n", "", false},
		{"over-bound", "---\nname: big\ndescription: " + strings.Repeat("x", skillFileLimit) + "\n---\n", "", false},
		{"cut-at-bound", head + pad + "---example\nmore\n", "", false},
		{"ends-at-bound", head + pad + "---", "x", true},
		{"long-body", "---\nname: long\n---\n" + strings.Repeat("body\n", skillFileLimit), "long", true},
	} {
		path := filepath.Join(dir, tc.name+".md")
		if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, ok := antigravitySkillFrontmatterName(path)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	if n := len(head + pad + "---"); n != skillFileLimit {
		t.Fatalf("the bound's fixture is %d bytes, want %d", n, skillFileLimit)
	}
	if _, ok := antigravitySkillFrontmatterName(dir); ok {
		t.Error("a directory read as a skill")
	}
	if _, ok := antigravitySkillFrontmatterName(filepath.Join(dir, "missing.md")); ok {
		t.Error("a missing file read as a skill")
	}
}
