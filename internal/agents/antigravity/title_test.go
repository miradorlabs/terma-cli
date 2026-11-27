package antigravity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// name writes the conversation's annotations as agy does, the name and the other fields it
// keeps beside it.
func (g *transcriptRig) name(body string) {
	g.t.Helper()
	path := antigravityAnnotations(&antigravityHookInput{ConversationID: g.id, TranscriptPath: g.transcript})
	if path == "" {
		g.t.Fatal("no annotations path for the rig's transcript")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		g.t.Fatal(err)
	}
}

// titles is the names spooled since the last drain.
func (g *transcriptRig) titles() []string {
	g.t.Helper()
	var out []string
	for _, e := range g.drain()[semconv.TermaSessionTitleEvent] {
		if e.SessionID != g.id || e.Attrs[semconv.GenAIMainAgentNameKey] != antigravityTool ||
			e.Attrs[semconv.TermaEvidenceSourceKey] != sourceAntigravityAnnotations {
			g.t.Errorf("title event %+v", e)
		}
		title, _ := e.Attrs[semconv.TermaSessionTitleKey].(string)
		out = append(out, title)
	}
	return out
}

// The name agy gave a conversation goes at the Stop of the turn it was given in, once, and a
// rename goes at the next turn's start.
func TestAntigravityTitleGoesOnceAndOnRename(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(1)
	g.hook(preInvocation, begin("1"))
	if got := g.titles(); len(got) != 0 {
		t.Fatalf("a title before agy named the conversation: %q", got)
	}
	g.name(`title:"Made-up Title"`)
	g.write(7)
	g.hook(postInvocation, invoked)
	if got := g.titles(); len(got) != 0 {
		t.Fatalf("PostInvocation sent a title: %q", got)
	}
	g.hook(stop, stopped)
	if got := g.titles(); strings.Join(got, ",") != "Made-up Title" {
		t.Fatalf("titles at Stop: %q", got)
	}

	// Unchanged: nothing more.
	g.write(8)
	g.hook(preInvocation, begin("8"))
	g.hook(stop, stopped)
	if got := g.titles(); len(got) != 0 {
		t.Fatalf("an unchanged title went again: %q", got)
	}

	// Renamed between turns, with the other fields agy keeps.
	g.name(`title:"Renamed \"made-up\" — title" tags:"a" archived:false last_user_view_time:{seconds:1}`)
	g.hook(preInvocation, begin("8"))
	if got := g.titles(); strings.Join(got, ",") != `Renamed "made-up" — title` {
		t.Fatalf("titles after a rename: %q", got)
	}
}

// A name agy gives after the turn's Stop goes at the next turn's start.
func TestAntigravityTitleAfterStopGoesNextTurn(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.turnOne()
	if got := g.titles(); len(got) != 0 {
		t.Fatalf("a title before agy named the conversation: %q", got)
	}
	g.name(`title:"Made-up Title"`)
	g.write(8)
	g.hook(preInvocation, begin("8"))
	if got := g.titles(); strings.Join(got, ",") != "Made-up Title" {
		t.Fatalf("titles at the next turn: %q", got)
	}
}

// The name restates the first prompt, so it goes only where prompts may.
func TestAntigravityTitleFollowsThePromptSwitch(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		noPrompts, noToolContent, deselect bool
		sent                               bool
	}{
		{name: "everything", sent: true},
		{name: "tool content off", noToolContent: true, sent: true},
		{name: "prompts off", noPrompts: true},
		{name: "Antigravity not chosen", deselect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
			g.noPrompts, g.noToolContent = tc.noPrompts, tc.noToolContent
			if tc.deselect {
				g.agents = []string{"codex"}
			}
			g.name(`title:"Made-up Title"`)
			g.turnOne()
			if got := g.titles(); (len(got) == 1) != tc.sent || len(got) > 1 {
				t.Errorf("titles %q, want sent %v", got, tc.sent)
			}
		})
	}
}

func TestReadAntigravityTitle(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, body string
		title      string
		found, err bool
	}{
		{name: "agy's shape", body: `title:"Council Design Review"`, title: "Council Design Review", found: true},
		{name: "spaced, multiline", body: "title: \"Made-up\"\npinned: true\n", title: "Made-up", found: true},
		{name: "escapes", body: `title:"tab\there \"q\" \303\251"`, title: "tab\there \"q\" é", found: true},
		{name: "unknown fields only", body: `archived:true tags:"x" last_user_view_time:{seconds:1 nanos:2}`},
		{name: "blank", body: `title:"  "`},
		{name: "empty file", body: ``},
		{name: "not text format", body: `{"title":"x"}`, err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".pbtxt")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			title, found, err := readAntigravityTitle(path)
			if title != tc.title || found != tc.found || (err != nil) != tc.err {
				t.Errorf("= %q, %v, %v; want %q, %v, error %v", title, found, err, tc.title, tc.found, tc.err)
			}
		})
	}
	if _, found, err := readAntigravityTitle(filepath.Join(dir, "missing.pbtxt")); found || err != nil {
		t.Errorf("a missing file: %v, %v", found, err)
	}
	if _, _, err := readAntigravityTitle(dir); err == nil {
		t.Error("a folder read as annotations")
	}
	long := filepath.Join(dir, "long.pbtxt")
	if err := os.WriteFile(long, []byte(`title:"`+strings.Repeat("x", antigravityAnnotationsMaxBytes)+`"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readAntigravityTitle(long); err == nil {
		t.Error("annotations past the bound were read")
	}
}

// The annotations sit beside the brain folder, under the conversation's own id; a transcript
// of another shape names none.
func TestAntigravityAnnotationsPath(t *testing.T) {
	id := turnsConversation
	home := filepath.Join(t.TempDir(), "antigravity-acp")
	transcript := filepath.Join(home, "brain", id, ".system_generated", "logs", "transcript.jsonl")
	if got := antigravityAnnotations(&antigravityHookInput{ConversationID: id, TranscriptPath: transcript}); got != filepath.Join(home, "annotations", id+".pbtxt") {
		t.Errorf("annotations %q", got)
	}
	if got := antigravityAnnotations(&antigravityHookInput{ConversationID: id, TranscriptPath: filepath.Join(home, "elsewhere", id, "transcript_full.jsonl")}); got != "" {
		t.Errorf("annotations for a transcript elsewhere: %q", got)
	}
}
