package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"strings"
)

// A skill a person invokes as $name reaches the model as a user message holding the skill
// whole, recorded only in the rollout; the model reading a skill itself runs a shell command,
// which its tool hook reports (shellSkillReads).
//
// Accepted risk: the rollout format is undocumented, and the record was observed on Codex
// 0.169.0 only, so a build that writes it differently sends no definition for a $name skill;
// a skill used only by running one of its scripts is not reported.

// codexSkillInstructions is the content kind Codex gives a $name skill's message.
const codexSkillInstructions = "skills.selected_skill_instructions"

// codexSkillHead opens that message's text.
const codexSkillHead = "<skill>\n<name>"

// skillCursor stores only the rollout's position and where its turn runs, never content.
type skillCursor struct {
	Identity string `json:"identity"`
	Offset   int64  `json:"offset"`
	Anchor   string `json:"anchor"`
	Skipping bool   `json:"skipping,omitempty"`
	turnState
}

// skillRecordMarks are what a record must hold to be decoded: a turn's place, or a skill.
var skillRecordMarks = [][]byte{
	[]byte("session_meta"), []byte("turn_context"), []byte("thread_settings_applied"),
	[]byte("task_started"), []byte("turn_started"), []byte(codexSkillInstructions),
}

// readRolloutSkills emits the path of each $name skill the rollout records from the cursor
// on, in turns the team collects, reading at most 1 MiB and 128 skills per call; the caller
// spools each before persisting the cursor.
func readRolloutSkills(ctx context.Context, sessionID, transcript string, cursor skillCursor, admits func(cwd string) bool, emit func(path string) error) (skillCursor, string, error) {
	f, status := openCodexRollout(ctx, sessionID, transcript)
	if f == nil {
		return cursor, status, nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return cursor, "unreadable", err
	}
	head := make([]byte, min(st.Size(), 64<<10))
	n, err := f.ReadAt(head, 0)
	if err != nil && err != io.EOF {
		return cursor, "unreadable", err
	}
	first, _, _ := bytes.Cut(head[:n], []byte{'\n'})
	identity := fundingHash(rolloutFileIdentity(st) + string(first))
	if cursor.Identity != identity || cursor.Offset < 0 || cursor.Offset > st.Size() ||
		(cursor.Offset > 0 && cursor.Anchor != cursorAnchor(f, cursor.Offset)) {
		cursor = skillCursor{Identity: identity}
	}
	cursor.recheck(admits)
	old := cursor
	checkpoint := func(s string, e error) (skillCursor, string, error) {
		cursor.Anchor = cursorAnchor(f, cursor.Offset)
		return cursor, s, e
	}
	data := make([]byte, min(int64(codexTailLimit), st.Size()-cursor.Offset))
	n, err = f.ReadAt(data, cursor.Offset)
	if err != nil && err != io.EOF {
		return old, "unreadable", err
	}
	data = data[:n]
	emitted := 0
	for len(data) > 0 {
		if ctx.Err() != nil {
			return checkpoint("backlog", ctx.Err())
		}
		if emitted >= 128 {
			return checkpoint("backlog", nil)
		}
		line, rest, complete := bytes.Cut(data, []byte{'\n'})
		if !complete {
			if cursor.Skipping {
				cursor.Offset += int64(len(data))
				return checkpoint("backlog", nil)
			}
			if len(data) < codexTailLimit {
				if cursor.Offset+int64(len(data)) < st.Size() {
					return checkpoint("backlog", nil)
				}
				return checkpoint("incomplete", nil)
			}
			// A record past the read's bound is skipped whole.
			cursor.Offset += int64(len(data))
			cursor.Skipping = true
			return checkpoint("backlog", nil)
		}
		if cursor.Skipping {
			cursor.Skipping = false
		} else if path, ok := codexSkillPathFrom(line, &cursor, admits); ok && cursor.Admitted {
			if err := emit(path); err != nil {
				return checkpoint("append_failed", err)
			}
			emitted++
		}
		cursor.Offset += int64(len(line) + 1)
		data = rest
	}
	if cursor.Offset < st.Size() {
		return checkpoint("backlog", nil)
	}
	return checkpoint("caught_up", nil)
}

// codexSkillPathFrom follows the record's turn, and returns the SKILL.md path a $name
// skill's message names; nothing else of the message is kept.
func codexSkillPathFrom(line []byte, cursor *skillCursor, admits func(string) bool) (string, bool) {
	if !slices.ContainsFunc(skillRecordMarks, func(m []byte) bool { return bytes.Contains(line, m) }) {
		return "", false
	}
	var rec struct {
		Type    string `json:"type"`
		Payload struct {
			Type string `json:"type"`
			Role string `json:"role"`
			// Content is decoded only for a user message: elsewhere it may be any shape.
			Content  json.RawMessage `json:"content"`
			Metadata struct {
				Kinds []string `json:"content_item_kinds"`
			} `json:"internal_chat_message_metadata_passthrough"`
			turnFields
		} `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return "", false
	}
	cursor.track(admits, rec.Type, rec.Payload.Type, rec.Payload.turnFields)
	p := rec.Payload
	if rec.Type != "response_item" || p.Type != "message" || p.Role != "user" || !slices.Contains(p.Metadata.Kinds, codexSkillInstructions) {
		return "", false
	}
	var content []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(p.Content, &content) != nil || len(content) == 0 || !strings.HasPrefix(content[0].Text, codexSkillHead) {
		return "", false
	}
	// <skill>, <name>…</name>, then <path>…</path>.
	lines := strings.SplitN(content[0].Text, "\n", 4)
	if len(lines) < 3 {
		return "", false
	}
	path, ok := strings.CutPrefix(lines[2], "<path>")
	path, closed := strings.CutSuffix(path, "</path>")
	if !ok || !closed || !filepath.IsAbs(path) {
		return "", false
	}
	return filepath.Clean(path), true
}
