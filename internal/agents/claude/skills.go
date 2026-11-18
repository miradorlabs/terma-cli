package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
)

// Claude Code says which skills a session used only in its transcript: the model's Skill tool
// calls, a person's /name commands, the directory each skill loaded from, and the listing of
// skills it gives the model, which is read only for its names, to tell a skill from a
// built-in command.
//
// Accepted risk: the transcript is undocumented and may change shape, and then no definitions
// are sent (nothing wrong is); a skill loaded in a way these records do not show (in a
// subagent's own transcript, or preloaded by an agent definition) gets none. Upgrade path: a
// documented skill event or hook from Claude Code.

// claudeSkillBaseDir opens the message Claude Code gives the model when a skill loads, before
// the skill's body.
const claudeSkillBaseDir = "Base directory for this skill: "

// claudeSkillListingsKept bounds how many listing records a cursor remembers: a session lists
// its skills once and again only as they change.
const claudeSkillListingsKept = 32

// claudeListingRecordLimit bounds the read of one listing record.
const claudeListingRecordLimit = 4 << 20

// claudeSkillUse is one use of a skill: by the model's Skill tool or a person's /name, with
// the directory Claude Code loaded it from when it said.
type claudeSkillUse struct {
	Name, Dir string
	toolUseID string
	slash     bool
}

// claudeSkillCursor is how far a transcript's skill uses were read, and where its skill
// listings are: byte offsets only, never the transcript's text.
type claudeSkillCursor struct {
	Offset   int64   `json:"offset"`
	Listings []int64 `json:"listings,omitempty"`
}

// claudeSkillRecord is the subset of a transcript record that says a skill was listed, called or loaded.
type claudeSkillRecord struct {
	Type   string `json:"type"`
	IsMeta bool   `json:"isMeta"`
	// Cwd is where the session ran when the record was written.
	Cwd             string `json:"cwd"`
	SourceToolUseID string `json:"sourceToolUseID"`
	Attachment      struct {
		Type  string   `json:"type"`
		Names []string `json:"names"`
	} `json:"attachment"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// claudeContentPart is one part of a message's content.
type claudeContentPart struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Text  string `json:"text"`
	Input struct {
		Skill string `json:"skill"`
	} `json:"input"`
	// ToolUseID and IsError are a tool result's: the call it answers, and whether it failed.
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
}

// readClaudeSkillUses returns the skills used in the transcript's whole lines from the
// cursor on, and the cursor after them, which also notes the listings it passed. A transcript
// shorter than the cursor is a new file: it is read from the start. Only a use written where
// admits says the team collects counts: a session that moved into a collected checkout still
// holds what it did before, and that stays on the machine.
//
// Accepted risk: a Skill call read before Claude Code wrote its base-directory record is
// reported without its directory, so without the origin, link or file's description, and
// not again this session. It takes a hook between the two records of one turn, and the
// transcript does not say a record is still to come: a failed call or a built-in skill
// writes none, so holding the cursor back would stall on those. Upgrade path: keep the open
// call ids in the cursor and report a call once its record, or the turn's end, is read.
func readClaudeSkillUses(path string, cur claudeSkillCursor, admits func(cwd string) bool) ([]claudeSkillUse, claudeSkillCursor, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, cur, nil
	}
	if err != nil {
		return nil, cur, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, cur, err
	}
	if cur.Offset > info.Size() {
		cur = claudeSkillCursor{}
	}
	if _, err := f.Seek(cur.Offset, io.SeekStart); err != nil {
		return nil, cur, err
	}
	var uses []claudeSkillUse
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			// A line without its newline is still being written.
			if errors.Is(err, io.EOF) {
				return uses, cur, nil
			}
			return uses, cur, err
		}
		at := cur.Offset
		cur.Offset += int64(len(line))
		if !mayNameSkill(line) {
			continue
		}
		var rec claudeSkillRecord
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if rec.Type == "attachment" && rec.Attachment.Type == "skill_listing" {
			// A listing is noted wherever it was written: it only tells which names are
			// skills, and a name leaves only through a use, judged where that was written.
			cur.Listings = append(cur.Listings, at)
			if len(cur.Listings) > claudeSkillListingsKept {
				// The first is the session's whole listing; the later ones only add to it.
				cur.Listings = slices.Delete(cur.Listings, 1, 2)
			}
			continue
		}
		if admits(rec.Cwd) {
			uses = rec.skillUses(uses)
		}
	}
}

// mayNameSkill keeps the transcript's other records from being decoded.
func mayNameSkill(line []byte) bool {
	for _, s := range []string{"skill_listing", `"Skill"`, "<command-name>", claudeSkillBaseDir, `"is_error":true`} {
		if bytes.Contains(line, []byte(s)) {
			return true
		}
	}
	return false
}

// skillUses adds what the record says to uses: a Skill call, a /name command, or the
// directory a skill loaded from, which goes to the call it names or else the last command.
// Nothing else of a loaded skill's message is kept: the rest is its body.
func (rec claudeSkillRecord) skillUses(uses []claudeSkillUse) []claudeSkillUse {
	var text string
	var parts []claudeContentPart
	if json.Unmarshal(rec.Message.Content, &text) != nil {
		_ = json.Unmarshal(rec.Message.Content, &parts)
	}
	switch {
	case rec.Type == "assistant":
		for _, p := range parts {
			if p.Type == "tool_use" && p.Name == "Skill" && p.Input.Skill != "" {
				uses = append(uses, claudeSkillUse{Name: strings.TrimPrefix(p.Input.Skill, "/"), toolUseID: p.ID})
			}
		}
	case rec.Type == "user" && rec.IsMeta:
		first := text
		if first == "" && len(parts) > 0 && parts[0].Type == "text" {
			first = parts[0].Text
		}
		dir, ok := strings.CutPrefix(first, claudeSkillBaseDir)
		if !ok {
			return uses
		}
		dir, _, _ = strings.Cut(dir, "\n")
		var u *claudeSkillUse
		if rec.SourceToolUseID != "" {
			for i := range uses {
				if uses[i].toolUseID == rec.SourceToolUseID {
					u = &uses[i]
				}
			}
		} else if n := len(uses); n > 0 && uses[n-1].slash {
			// A command's skill loads right after its <command-name> record, so a record that
			// names no call goes only to the last use, and only when that use is a command.
			u = &uses[n-1]
		}
		if u != nil && u.Dir == "" {
			u.Dir = strings.TrimSpace(dir)
		}
	case rec.Type == "user":
		texts := []string{text}
		for _, p := range parts {
			switch {
			case p.Type == "text":
				texts = append(texts, p.Text)
			case p.Type == "tool_result" && p.IsError && p.ToolUseID != "":
				// A Skill call Claude Code refused (an unknown skill, a denied one) used none.
				uses = slices.DeleteFunc(uses, func(u claudeSkillUse) bool { return u.toolUseID == p.ToolUseID })
			}
		}
		for _, t := range texts {
			if name, ok := commandName(t); ok {
				uses = append(uses, claudeSkillUse{Name: name, slash: true})
			}
		}
	}
	return uses
}

// commandName is the /name a person's message ran, without its slash.
func commandName(text string) (string, bool) {
	_, rest, ok := strings.Cut(text, "<command-name>/")
	if !ok {
		return "", false
	}
	name, _, ok := strings.Cut(rest, "</command-name>")
	return name, ok && name != "" && !strings.ContainsAny(name, " \n<>")
}

// readClaudeSkillListings is the set of names the listings at the transcript's offsets hold.
// Only the names are read: a listing's descriptions mix what authors wrote with lines Claude
// Code took from skills' bodies, and nothing in the transcript tells the two apart.
func readClaudeSkillListings(path string, offsets []int64) map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer func() { _ = f.Close() }()
	for _, off := range offsets {
		line, err := bufio.NewReader(io.NewSectionReader(f, off, claudeListingRecordLimit)).ReadBytes('\n')
		if err != nil {
			continue
		}
		var rec claudeSkillRecord
		if json.Unmarshal(line, &rec) != nil || rec.Attachment.Type != "skill_listing" {
			continue
		}
		for _, n := range rec.Attachment.Names {
			out[n] = true
		}
	}
	return out
}
