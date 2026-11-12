package claude

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/promptimage"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// claudePixel is a 1×1 PNG, as Claude Code records a pasted image.
const claudePixel = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

// pastedRecord is a user record typed in cwd whose prompt holds text and the given images,
// numbered as Claude Code numbers pastes.
func pastedRecord(cwd, uuid, promptID string, pasteIDs []int, images ...string) string {
	content := []any{map[string]any{"type": "text", "text": "[Image #1] what is this"}}
	for _, data := range images {
		content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": data}})
	}
	b, _ := json.Marshal(map[string]any{
		"type": "user", "uuid": uuid, "promptId": promptID, "cwd": cwd, "timestamp": "2026-11-09T10:00:00Z",
		"imagePasteIds": pasteIDs, "message": map[string]any{"role": "user", "content": content},
	})
	return string(b)
}

func spooledImages(t *testing.T, sp *spool.Queue) []spool.Event {
	t.Helper()
	var out []spool.Event
	for _, e := range hookruntest.Queued(t, sp) {
		if e.Name == semconv.TermaPromptImageEvent {
			out = append(out, e)
		}
	}
	return out
}

// Stop spools one record per image the user put in a prompt, its bytes kept aside for
// delivery; a tool's screenshot and a subagent's records are not the user's, and a later
// stop spools only what was recorded since.
func TestClaudeStopSpoolsEachPastedImageOnce(t *testing.T) {
	env := titleEnv(t)
	other := base64.StdEncoding.EncodeToString(append(mustDecode(t, claudePixel), 0))
	path := transcript(t, t.TempDir(),
		`{"type":"user","uuid":"u0","message":{"role":"user","content":"no image here"}}`,
		pastedRecord(env.Cwd, "u1", "p1", []int{1, 2}, claudePixel, other),
		// A screenshot a tool read: its image sits inside the tool's result.
		`{"type":"user","uuid":"u2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+claudePixel+`"}}]}]}}`,
		`{"type":"user","uuid":"u3","isSidechain":true,"message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+claudePixel+`"}}]}}`,
	)
	stopClaude(t, env, path)
	if images := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaPromptImageEvent); len(images) != 0 {
		t.Fatalf("%d images went to the events' spool", len(images))
	}
	got := spooledImages(t, env.Images)
	if len(got) != 2 {
		t.Fatalf("spooled %d images, want the prompt's two: %+v", len(got), got)
	}
	first := got[0].Attrs
	img, _ := promptimage.FromBase64("image/png", claudePixel)
	for k, want := range map[string]any{
		semconv.GenAIMainAgentNameKey:  claudeTool,
		semconv.TermaEvidenceSourceKey: sourceClaudeTranscript,
		semconv.TermaMessageIDKey:      "u1",
		semconv.TermaPromptIDKey:       "p1",
		semconv.TermaImageIDKey:        img.ID(),
		semconv.TermaImageIndexKey:     float64(1),
		semconv.TermaImageLabelKey:     "[Image #1]",
		semconv.TermaImageMimeTypeKey:  "image/png",
		semconv.TermaImageWidthKey:     float64(1),
	} {
		if first[k] != want {
			t.Errorf("%s = %v, want %v", k, first[k], want)
		}
	}
	if _, ok := first[semconv.TermaImageDataKey]; ok {
		t.Error("the image's bytes went into the spool")
	}
	if got[1].Attrs[semconv.TermaImageIndexKey] != float64(2) || got[1].Attrs[semconv.TermaImageLabelKey] != "[Image #2]" {
		t.Errorf("second image = %v", got[1].Attrs)
	}
	if data, ok := promptimage.Load(env.StateDir, img.ID()); !ok || data != claudePixel {
		t.Fatal("the image's bytes were not kept for delivery")
	}

	appendTranscript(t, path, pastedRecord(env.Cwd, "u4", "p2", []int{3}, claudePixel)+"\n")
	stopClaude(t, env, path)
	got = spooledImages(t, env.Images)
	if len(got) != 1 || got[0].Attrs[semconv.TermaMessageIDKey] != "u4" || got[0].Attrs[semconv.TermaImageLabelKey] != "[Image #3]" {
		t.Fatalf("second stop: got %+v, want u4's image alone", got)
	}
}

// Without the developer's consent for Claude Code, no image is read.
func TestClaudeImagesNeedConsent(t *testing.T) {
	env := titleEnv(t)
	env.Agents = nil
	path := transcript(t, t.TempDir(), pastedRecord(env.Cwd, "u1", "p1", []int{1}, claudePixel))
	stopClaude(t, env, path)
	if got := spooledImages(t, env.Images); len(got) != 0 {
		t.Fatalf("spooled %+v without consent", got)
	}
}

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An image that cannot be read keeps its place: the next one is still the prompt's second,
// under its own placeholder.
func TestClaudeImagesKeepTheirPlace(t *testing.T) {
	p, ok := claudePromptFrom([]byte(pastedRecord("", "u1", "p1", []int{3, 4}, "!!not base64", claudePixel)))
	if !ok || len(p.Images) != 1 {
		t.Fatalf("prompt = %+v, %v", p, ok)
	}
	if img := p.Images[0]; img.Index != 2 || img.Label != "[Image #4]" {
		t.Fatalf("image = index %d label %q", img.Index, img.Label)
	}
}

// A team that does not collect prompts never gets the image, so nothing of it is kept: no
// record and no bytes.
func TestClaudeImagesKeepNothingWhenPromptsAreOff(t *testing.T) {
	env := titleEnv(t)
	env.Policy.IncludePrompts = false
	path := transcript(t, t.TempDir(), pastedRecord(env.Cwd, "u1", "p1", []int{1}, claudePixel))
	stopClaude(t, env, path)
	if got := spooledImages(t, env.Images); len(got) != 0 {
		t.Fatalf("spooled %d images the team does not collect", len(got))
	}
	img, _ := promptimage.FromBase64("image/png", claudePixel)
	if _, ok := promptimage.Load(env.StateDir, img.ID()); ok {
		t.Fatal("bytes the team does not collect were kept")
	}
}

// A session moved into a listed repository, or resumed there, still holds what was typed
// before elsewhere: an image typed where the team does not collect stays on the machine, and
// the read moves past it.
func TestClaudeImagesStayHomeWhereTheTeamDoesNotCollect(t *testing.T) {
	env := titleEnv(t)
	path := transcript(t, t.TempDir(),
		pastedRecord(t.TempDir(), "u1", "p1", []int{1}, claudePixel),
		pastedRecord("", "u2", "p2", []int{2}, claudePixel),
		pastedRecord(env.Cwd, "u3", "p3", []int{3}, claudePixel),
	)
	stopClaude(t, env, path)
	got := spooledImages(t, env.Images)
	if len(got) != 1 || got[0].Attrs[semconv.TermaMessageIDKey] != "u3" {
		t.Fatalf("spooled %+v, want u3's image alone", got)
	}
	stopClaude(t, env, path)
	if got := spooledImages(t, env.Images); len(got) != 0 {
		t.Fatalf("a second stop spooled %+v", got)
	}
}
