package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/promptimage"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// codexPixel is a 1×1 PNG as Codex records it, a data URL.
const codexPixel = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

// imagePrompt is a user message as Codex records a prompt with a pasted image (labelled,
// between its tags) and one given on the command line (unlabelled).
func imagePrompt(id string) string {
	return fmt.Sprintf(`{"timestamp":"2026-09-19T18:18:39.200Z","type":"response_item","payload":{"type":"message","role":"user","id":%q,"content":[`+
		`{"type":"input_text","text":"<image name=[Image #1] path=\"/tmp/codex-clipboard-a.png\">"},{"type":"input_image","image_url":%q,"detail":"high"},{"type":"input_text","text":"</image>"},`+
		`{"type":"input_image","image_url":%q},{"type":"input_text","text":"[Image #1] what is this"}]}}`+"\n", id, codexPixel, codexPixel)
}

func readImages(t *testing.T, path string, c imageCursor, admits func(string) bool) (imageCursor, []codexPrompt) {
	t.Helper()
	var out []codexPrompt
	next, err := readRolloutImages(context.Background(), testCodexID, path, c, admits, func(p codexPrompt) error { out = append(out, p); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return next, out
}

// A prompt's images come out in its turn, labelled as its text shows them; an assistant's
// message is no prompt, and a re-read from the cursor finds nothing new.
func TestCodexImagesCarryTheirTurnAndLabel(t *testing.T) {
	path := sequenceFixture(t,
		replyTaskStarted(replyTurnA, replyTraceA)+replyTurnContext(replyTurnA)+imagePrompt("msg_u1")+
			strings.Replace(imagePrompt("msg_a1"), `"role":"user"`, `"role":"assistant"`, 1)+
			replyTaskComplete(replyTurnA))
	cursor, prompts := readImages(t, path, imageCursor{}, admitAll)
	if len(prompts) != 1 {
		t.Fatalf("prompts %+v", prompts)
	}
	p := prompts[0]
	if p.ID != "msg_u1" || p.TurnID != replyTurnA || p.TraceID != replyTraceA || len(p.Images) != 2 {
		t.Fatalf("prompt = %+v", p)
	}
	want, _ := promptimage.FromDataURL(codexPixel)
	for i, label := range []string{"[Image #1]", ""} {
		img := p.Images[i]
		if img.Index != i+1 || img.Label != label || img.ID() != want.ID() || img.MIMEType != "image/png" {
			t.Errorf("image %d = index %d label %q type %s", i, img.Index, img.Label, img.MIMEType)
		}
	}
	if _, again := readImages(t, path, cursor, admitAll); len(again) != 0 {
		t.Fatalf("re-read %+v", again)
	}
	appendRollout(t, path, replyTaskStarted(replyTurnB, replyTraceB)+replyTurnContext(replyTurnB)+imagePrompt("msg_u2"))
	if _, more := readImages(t, path, cursor, admitAll); len(more) != 1 || more[0].ID != "msg_u2" || more[0].TraceID != replyTraceB {
		t.Fatalf("appended prompt = %+v", more)
	}
}

// A turn the team does not collect keeps its images home.
func TestCodexImagesFollowTurnAdmission(t *testing.T) {
	path := sequenceFixture(t,
		replyTaskStarted(replyTurnA, replyTraceA)+replyTurnContext(replyTurnA)+imagePrompt("msg_u1"))
	if _, prompts := readImages(t, path, imageCursor{}, func(string) bool { return false }); len(prompts) != 0 {
		t.Fatalf("an uncollected turn's images = %+v", prompts)
	}
}

// A message whose images could not be spooled is read again.
func TestCodexImagesAreNotAcknowledgedUntilSpooled(t *testing.T) {
	path := sequenceFixture(t, replyTaskStarted(replyTurnA, replyTraceA)+imagePrompt("msg_u1"))
	failed := errors.New("spool full")
	cursor, err := readRolloutImages(context.Background(), testCodexID, path, imageCursor{}, admitAll, func(codexPrompt) error { return failed })
	if !errors.Is(err, failed) {
		t.Fatalf("err = %v", err)
	}
	if _, prompts := readImages(t, path, cursor, admitAll); len(prompts) != 1 {
		t.Fatalf("after a failed spool, re-read %+v", prompts)
	}
}

// With prompts consented, a turn's end spools each image the user put in its prompt under
// the turn's trace id, its bytes kept aside for delivery rather than in the spool.
func TestCodexStopSpoolsPromptImages(t *testing.T) {
	env := fundingEnv(t)
	routeCodex(t, &env)
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "19", "rollout-2026-09-19T12-18-12-"+replySession+".jsonl")
	hookruntest.WriteFile(t, filepath.Dir(path), filepath.Base(path), strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"` + replySession + `","cwd":` + strconv.Quote(env.Cwd) + `}}`,
		`{"timestamp":"2026-09-19T18:18:39.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"` + replyTurn + `","trace_id":"` + replyTrace + `"}}`,
		strings.TrimSuffix(turnContext(replyTurn, env.Cwd), "\n"),
		strings.TrimSuffix(imagePrompt("msg_u1"), "\n"),
		`{"timestamp":"2026-09-19T18:19:48.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"` + replyTurn + `"}}`,
	}, "\n")+"\n")
	b, _ := json.Marshal(map[string]any{"session_id": replySession, "cwd": env.Cwd, "transcript_path": path})
	env.Stdin = strings.NewReader(string(b))
	if err := stop(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	images := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaPromptImageEvent)
	if len(images) != 2 {
		t.Fatalf("spooled %d images, want the prompt's two", len(images))
	}
	first := images[0]
	img, _ := promptimage.FromDataURL(codexPixel)
	for k, want := range map[string]any{
		semconv.GenAIMainAgentNameKey: codexTool, semconv.TermaEvidenceSourceKey: sourceCodexRollout,
		semconv.TermaMessageIDKey: "msg_u1", semconv.TermaTurnIDKey: replyTurn,
		semconv.TermaImageIDKey: img.ID(), semconv.TermaImageLabelKey: "[Image #1]", semconv.TermaImageIndexKey: float64(1),
	} {
		if first.Attrs[k] != want {
			t.Errorf("%s = %v, want %v", k, first.Attrs[k], want)
		}
	}
	if first.TraceID != replyTrace || first.SessionID != replySession {
		t.Errorf("image in trace %q session %q", first.TraceID, first.SessionID)
	}
	if _, ok := first.Attrs[semconv.TermaImageDataKey]; ok {
		t.Error("the image's bytes went into the spool")
	}
	if _, ok := promptimage.Load(env.StateDir, img.ID()); !ok {
		t.Error("the image's bytes were not kept for delivery")
	}
}
