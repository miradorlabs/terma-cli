package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/promptimage"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Claude Code's prompt event and its UserPromptSubmit hook carry a pasted image as its
// placeholder alone ("[Image #1]"); only the transcript keeps the image, as a base64 block
// beside the prompt's text in the user's record. A tool's image (a screenshot it read) sits
// inside that tool's result block, so a record's own image blocks are what the user put in.

// claudePromptImageStateDir holds how far each transcript's images have been read.
var claudePromptImageStateDir = hookrun.AgentStateDir(name, "prompt-images")

// claudeUserRecord is the subset of a user transcript record readPromptImages reads.
type claudeUserRecord struct {
	Type          string    `json:"type"`
	UUID          string    `json:"uuid"`
	PromptID      string    `json:"promptId"`
	IsSidechain   bool      `json:"isSidechain"`
	Timestamp     time.Time `json:"timestamp"`
	ImagePasteIDs []int     `json:"imagePasteIds"`
	Message       struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// claudePrompt is one user record's images.
type claudePrompt struct {
	UUID, PromptID string
	Time           time.Time
	Images         []promptimage.Image
}

// promptImageState is the transcript offset up to which images were spooled.
type promptImageState struct {
	Offset int64 `json:"offset"`
}

// captureClaudePromptImages spools one terma.prompt.image per image the user put in a
// prompt since the last read to the image lane, its bytes kept aside for delivery, when a
// turn stops or fails or the session ends; the relay sends them. They leave under the
// title's consent while the team collects prompts.
func captureClaudePromptImages(e hookrun.Env, r *hookrun.Repo, sessionID, transcript string) {
	if transcript == "" || !session.ValidID(sessionID) || !titleConsented(e.Consent()) {
		return
	}
	dir := filepath.Join(e.StateDir, claudePromptImageStateDir)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	path := filepath.Join(dir, hookrun.EvidenceID(transcript)+".json")
	// A hook already reading this transcript spools what this one would.
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var last promptImageState
	b, readErr := os.ReadFile(path)
	if readErr == nil && json.Unmarshal(b, &last) != nil {
		last = promptImageState{}
	}
	collect, _ := e.Policy.Content()
	spooled := 0
	offset, err := readPromptImages(transcript, last.Offset, func(p claudePrompt) bool {
		// A team that does not collect prompts never gets the image: nothing of it is kept.
		if !collect {
			return true
		}
		// A record with no id the platform can take is skipped before its bytes are kept.
		ids := map[string]any{}
		hookrun.BoundedAttr(ids, semconv.TermaMessageIDKey, p.UUID)
		if ids[semconv.TermaMessageIDKey] == nil {
			return true
		}
		hookrun.BoundedAttr(ids, semconv.TermaPromptIDKey, p.PromptID)
		for _, img := range p.Images {
			// A store that cannot keep the bytes still lets the record say what the image was.
			attrs, err := promptimage.Keep(e.StateDir, img, collect)
			if err != nil {
				e.Logf("prompt image: %v", err)
			}
			attrs[semconv.GenAIMainAgentNameKey] = claudeTool
			attrs[semconv.TermaEvidenceSourceKey] = sourceClaudeTranscript
			maps.Copy(attrs, ids)
			// Unsent images stay past the stored offset, for the next stop; the platform folds a re-sent one away.
			if !e.EmitImageFor(r, spool.Event{Time: p.Time, Name: semconv.TermaPromptImageEvent, SessionID: sessionID, Attrs: attrs}) {
				return false
			}
			spooled++
		}
		return true
	})
	if err != nil {
		e.Logf("prompt images: %v", err)
	}
	if offset != last.Offset {
		if b, err := json.Marshal(promptImageState{Offset: offset}); err == nil {
			if err := hookrun.WriteState(path, b); err != nil {
				e.Logf("prompt image state: %v", err)
			}
		}
	}
	if os.IsNotExist(readErr) {
		hookrun.PruneState(dir, e.Time().Add(-spool.MaxAge))
		promptimage.Prune(e.StateDir, e.Time().Add(-spool.MaxAge))
	}
}

// readPromptImages hands emit each user record with images in the transcript's whole lines
// from offset on, and returns the offset after the last line it read; a record emit
// refused is not read, so the next read starts at it.
func readPromptImages(path string, offset int64, emit func(claudePrompt) bool) (int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return offset, nil
	}
	if err != nil {
		return offset, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return offset, err
	}
	if offset > info.Size() {
		// A transcript shorter than what was read is a new file at the same path.
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			// A line without its newline is still being written.
			if errors.Is(err, io.EOF) {
				return offset, nil
			}
			return offset, err
		}
		if p, ok := claudePromptFrom(line); ok && !emit(p) {
			return offset, nil
		}
		offset += int64(len(line))
	}
}

// claudePromptFrom is the images of a user's own record, false for any other record or one
// with none.
func claudePromptFrom(line []byte) (claudePrompt, bool) {
	if !bytes.Contains(line, []byte(`"image"`)) || !bytes.Contains(line, []byte(`"user"`)) {
		return claudePrompt{}, false
	}
	// A tool's result (a screenshot it read, its base64 twice over) is stepped over undecoded,
	// unless the record also numbers pastes of its own.
	if bytes.Contains(line, []byte(`"tool_use_id"`)) && !bytes.Contains(line, []byte(`"imagePasteIds"`)) {
		return claudePrompt{}, false
	}
	var rec claudeUserRecord
	if json.Unmarshal(line, &rec) != nil || rec.Type != "user" || rec.IsSidechain || rec.Message.Role != "user" {
		return claudePrompt{}, false
	}
	var blocks []struct {
		Type   string `json:"type"`
		Source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
	}
	// A prompt of text alone is a string, not blocks.
	if json.Unmarshal(rec.Message.Content, &blocks) != nil {
		return claudePrompt{}, false
	}
	p := claudePrompt{UUID: rec.UUID, PromptID: rec.PromptID, Time: rec.Timestamp}
	// n counts the record's image blocks, one that cannot be read included, so each image
	// keeps its own position and placeholder.
	n := 0
	for _, b := range blocks {
		if b.Type != "image" {
			continue
		}
		n++
		if b.Source.Type != "base64" {
			continue
		}
		img, ok := promptimage.FromBase64(b.Source.MediaType, b.Source.Data)
		if !ok {
			continue
		}
		img.Index = n
		// imagePasteIds numbers the record's images as its text's placeholders do.
		if i := n - 1; i < len(rec.ImagePasteIDs) && rec.ImagePasteIDs[i] > 0 {
			img.Label = fmt.Sprintf("[Image #%d]", rec.ImagePasteIDs[i])
		}
		p.Images = append(p.Images, img)
	}
	return p, len(p.Images) > 0
}
