package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/promptimage"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Codex's prompt event and its UserPromptSubmit hook carry an image as its placeholder
// alone ("[Image #1]") and its export only counts them; the rollout keeps each image the user
// put in a prompt as a data URL in the user's message, after an `<image name=[Image #1]
// path=…>` tag when it has a label. The rollout is already read for replies, under the same
// consent: what the user said and what Codex answered travel together or not at all.

// codexImageCursorDir holds how far each rollout's images have been read.
var codexImageCursorDir = hookrun.AgentStateDir(name, "prompt-images")

// codexImageLabel is the label in a local image's opening tag.
var codexImageLabel = regexp.MustCompile(`^<image name=(\[Image #\d+\])`)

// imageCursor holds no image: an offset, the rollout's identity, and the turn in progress.
type imageCursor struct {
	Identity string `json:"identity"`
	Offset   int64  `json:"offset"`
	TurnID   string `json:"turn_id,omitempty"`
	TraceID  string `json:"trace_id,omitempty"`
	turnState
}

// codexPrompt is one user message's images, with the turn it opened.
type codexPrompt struct {
	ID, TurnID, TraceID string
	At                  time.Time
	Images              []promptimage.Image
}

// errImageNotKept is an image record neither spooled nor parked: the rollout is read again.
var errImageNotKept = errors.New("the image record could not be kept")

// captureCodexImages spools one terma.prompt.image per image the user put in a prompt since
// the last capture, from the turns the team collects, to the image lane, its bytes kept
// aside for delivery, which sends them only while the team's policy collects prompts.
func captureCodexImages(ctx context.Context, e hookrun.Env, r *hookrun.Repo, in *codexHookInput) {
	if !session.ValidID(in.SessionID) || !repliesConsented(e.Consent()) {
		return
	}
	dir := filepath.Join(e.StateDir, codexImageCursorDir)
	rollout := codexRolloutID(in)
	path := filepath.Join(dir, hookrun.EvidenceID(rollout)+".json")
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var cursor imageCursor
	b, readErr := os.ReadFile(path)
	if readErr == nil && json.Unmarshal(b, &cursor) != nil {
		cursor = imageCursor{}
	}
	ctx, cancel := context.WithTimeout(ctx, codexCaptureTimeout)
	defer cancel()
	collect, _ := e.Policy.Content()
	spooled := 0
	next, err := readRolloutImages(ctx, rollout, in.TranscriptPath, cursor, turnAdmission(ctx, e), func(p codexPrompt) error {
		// A team that does not collect prompts never gets the image: nothing of it is kept.
		if !collect {
			return nil
		}
		for _, img := range p.Images {
			// A store that cannot keep the bytes still lets the record say what the image was.
			attrs, err := promptimage.Keep(e.StateDir, img, collect)
			if err != nil {
				e.Logf("prompt image: %v", err)
			}
			attrs[semconv.GenAIMainAgentNameKey] = codexTool
			attrs[semconv.TermaEvidenceSourceKey] = sourceCodexRollout
			attrs[semconv.TermaMessageIDKey] = p.ID
			attrs = hookrun.AgentAttrs(attrs, in.AgentID, in.AgentType)
			hookrun.BoundedAttr(attrs, semconv.TermaTurnIDKey, p.TurnID)
			// Under the turn's trace id, as its replies are, so the platform files it in that turn.
			ev := spool.Event{Time: p.At, Name: semconv.TermaPromptImageEvent, SessionID: in.SessionID, TraceID: p.TraceID, Attrs: attrs}
			if ev.Time.IsZero() || ev.Time.After(e.Time()) {
				ev.Time = e.Time()
			}
			if !e.EmitImageFor(r, ev) {
				return errImageNotKept
			}
			spooled++
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		e.Logf("codex images: %v", err)
	}
	if next != cursor {
		if b, err := json.Marshal(next); err == nil {
			if err := hookrun.WriteState(path, b); err != nil {
				e.Logf("image cursor: %v", err)
			}
		}
	}
	if os.IsNotExist(readErr) {
		hookrun.PruneState(dir, e.Time().Add(-spool.MaxAge))
		promptimage.Prune(e.StateDir, e.Time().Add(-spool.MaxAge))
	}
}

// readRolloutImages hands emit each user message with images in the rollout's whole lines
// since cursor, from the turns admits collects, and returns the cursor to persist: after the
// last line read, so a message emit refused, or one the deadline cut off, is read again.
func readRolloutImages(ctx context.Context, sessionID, transcript string, cursor imageCursor, admits func(string) bool, emit func(codexPrompt) error) (imageCursor, error) {
	f, _ := openCodexRollout(ctx, sessionID, transcript)
	if f == nil {
		return cursor, nil
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return cursor, err
	}
	head := make([]byte, min(st.Size(), 64<<10))
	n, err := f.ReadAt(head, 0)
	if err != nil && err != io.EOF {
		return cursor, err
	}
	first, _, _ := bytes.Cut(head[:n], []byte{'\n'})
	// A rewritten or replaced rollout is read from its start.
	if identity := fundingHash(rolloutFileIdentity(st) + string(first)); cursor.Identity != identity || cursor.Offset < 0 || cursor.Offset > st.Size() {
		cursor = imageCursor{Identity: identity}
	}
	cursor.recheck(admits)
	if _, err := f.Seek(cursor.Offset, io.SeekStart); err != nil {
		return cursor, err
	}
	br := bufio.NewReader(f)
	for {
		if err := ctx.Err(); err != nil {
			return cursor, err
		}
		line, err := br.ReadBytes('\n')
		if err != nil {
			// A line without its newline is still being written.
			if errors.Is(err, io.EOF) {
				return cursor, nil
			}
			return cursor, err
		}
		next := cursor
		if p, ok := codexPromptFrom(line, &next, sessionID, admits); ok && next.Admitted {
			if err := emit(p); err != nil {
				return cursor, err
			}
		}
		next.Offset += int64(len(line))
		cursor = next
	}
}

// codexRecordKinds are the records readRolloutImages decodes: those that move the turn, and
// a user's message with an image. Every other line, a tool's output among
// them, is stepped over undecoded, its images too.
var codexRecordKinds = [][]byte{
	[]byte(`"turn_context"`), []byte(`"session_meta"`), []byte(`"thread_settings_applied"`),
	[]byte(`"task_started"`), []byte(`"turn_started"`), []byte(`"task_complete"`), []byte(`"turn_complete"`), []byte(`"turn_aborted"`),
}

// codexPromptFrom keeps the cursor's turn current and returns the images of a user's
// message; an unparseable record is left alone.
func codexPromptFrom(line []byte, cursor *imageCursor, sessionID string, admits func(string) bool) (codexPrompt, bool) {
	relevant := bytes.Contains(line, []byte(`"input_image"`)) && bytes.Contains(line, []byte(`"role":"user"`))
	for _, kind := range codexRecordKinds {
		if bytes.Contains(line, kind) {
			relevant = true
			break
		}
	}
	if !relevant {
		return codexPrompt{}, false
	}
	var rec struct {
		Timestamp time.Time `json:"timestamp"`
		Type      string    `json:"type"`
		Payload   struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			ID      string `json:"id"`
			TurnID  string `json:"turn_id"`
			TraceID string `json:"trace_id"`
			turnFields
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL string `json:"image_url"`
			} `json:"content"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return codexPrompt{}, false
	}
	cursor.track(admits, rec.Type, rec.Payload.Type, rec.Payload.turnFields)
	switch {
	case rec.Type == "turn_context", rec.Type == "event_msg" && (rec.Payload.Type == "task_started" || rec.Payload.Type == "turn_started"):
		// turn_context repeats the turn id without its trace id; only a different turn ends this one.
		if rec.Payload.TurnID != cursor.TurnID {
			cursor.TurnID, cursor.TraceID = "", ""
			if hookrun.EvidenceLabel.MatchString(rec.Payload.TurnID) {
				cursor.TurnID = rec.Payload.TurnID
			}
		}
		if spool.ValidTraceID(rec.Payload.TraceID) {
			cursor.TraceID = rec.Payload.TraceID
		}
		return codexPrompt{}, false
	case rec.Type == "event_msg" && (rec.Payload.Type == "task_complete" || rec.Payload.Type == "turn_complete" || rec.Payload.Type == "turn_aborted"):
		cursor.TurnID, cursor.TraceID = "", ""
		return codexPrompt{}, false
	case rec.Type != "response_item" || rec.Payload.Type != "message" || rec.Payload.Role != "user":
		return codexPrompt{}, false
	}
	p := codexPrompt{ID: rec.Payload.ID, TurnID: cursor.TurnID, TraceID: cursor.TraceID, At: rec.Timestamp}
	if !hookrun.EvidenceLabel.MatchString(p.ID) {
		p.ID = fundingHash(sessionID + cursor.Identity + fmt.Sprint(cursor.Offset))
	}
	label, n := "", 0
	for _, part := range rec.Payload.Content {
		switch part.Type {
		case "input_text":
			label = ""
			if m := codexImageLabel.FindStringSubmatch(part.Text); m != nil {
				label = m[1]
			}
		case "input_image":
			// n counts every image part, one that cannot be read included, so each keeps its position.
			n++
			img, ok := promptimage.FromDataURL(part.ImageURL)
			if !ok {
				label = ""
				continue
			}
			img.Index, img.Label = n, label
			label = ""
			p.Images = append(p.Images, img)
		}
	}
	return p, len(p.Images) > 0
}
