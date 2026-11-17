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
// calls, a person's /name commands, and the listing of skills it gives the model.
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
		Type    string   `json:"type"`
		Content string   `json:"content"`
		Names   []string `json:"names"`
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
}

// readClaudeSkillUses returns the skills used in the transcript's whole lines from the
// cursor on, and the cursor after them, which also notes the listings it passed. A transcript
// shorter than the cursor is a new file: it is read from the start. Only a record written
// where admits says the team collects counts, use or listing: a session that moved into a
// collected checkout still holds what it did before, and that stays on the machine.
//
// Accepted risk: a Skill call read before Claude Code wrote its base-directory record is
// reported without its directory, so without the origin, link or file's description, and
// not again this session. It takes a hook between the two records of one turn, and the
// transcript does not say a record is still to come: a built-in skill writes none, so
// holding the cursor back would stall on those. Upgrade path: keep the open call ids in
// the cursor and report a call once its record, or the turn's end, is read.
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
		if json.Unmarshal(line, &rec) != nil || !admits(rec.Cwd) {
			continue
		}
		if rec.Type == "attachment" && rec.Attachment.Type == "skill_listing" {
			cur.Listings = append(cur.Listings, at)
			if len(cur.Listings) > claudeSkillListingsKept {
				// The first is the session's whole listing; the later ones only add to it.
				cur.Listings = slices.Delete(cur.Listings, 1, 2)
			}
			continue
		}
		uses = rec.skillUses(uses)
	}
}

// mayNameSkill keeps the transcript's other records from being decoded.
func mayNameSkill(line []byte) bool {
	for _, s := range []string{"skill_listing", `"Skill"`, "<command-name>", claudeSkillBaseDir} {
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
		for i := len(uses) - 1; i >= 0; i-- {
			u := &uses[i]
			if u.Dir == "" && (rec.SourceToolUseID != "" && u.toolUseID == rec.SourceToolUseID || rec.SourceToolUseID == "" && u.slash) {
				u.Dir = strings.TrimSpace(dir)
				break
			}
		}
	case rec.Type == "user":
		texts := []string{text}
		for _, p := range parts {
			if p.Type == "text" {
				texts = append(texts, p.Text)
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

// claudeListed is a skill's entry in a listing: Shortened when Claude Code cut its
// description, or its edges in the listing cannot be told, so it is not the skill's whole one.
type claudeListed struct {
	Description string
	Shortened   bool
}

// whole reports whether the entry holds the skill's whole description.
func (l claudeListed) whole() bool { return l.Description != "" && !l.Shortened }

// parseSkillListing reads a listing's content, `- name: description` entries in the order of
// names. A description may span lines, so entries are found by name, each at a line's start;
// one may carry a display name in parentheses after its name. An entry the listing left
// without a description, to fit its budget, has none.
func parseSkillListing(content string, names []string) map[string]claudeListed {
	starts := make([]int, len(names))
	// unsure are the entries whose edges cannot be told: a name that opens a second line,
	// as a line of a description can, and the entry before it, which that line may belong to.
	unsure := make([]bool, len(names))
	from, last := 0, -1
	for i, n := range names {
		starts[i] = listingEntry(content, n, from)
		if starts[i] < 0 {
			continue
		}
		from = starts[i] + 1
		if listingEntry(content, n, from) >= 0 {
			unsure[i] = true
			if last >= 0 {
				unsure[last] = true
			}
		}
		last = i
	}
	out := map[string]claudeListed{}
	for i, n := range names {
		if starts[i] < 0 {
			continue
		}
		end := len(content)
		for _, s := range starts[i+1:] {
			if s >= 0 {
				end = s - 1 // the newline before the next entry
				break
			}
		}
		begin := starts[i] + len("- ") + len(n)
		if end < begin {
			continue // a name that spans lines: no entry to read
		}
		entry := content[begin:end]
		if strings.HasPrefix(entry, " (") {
			if paren := strings.Index(entry, ")"); paren >= 0 && !strings.Contains(entry[:paren], "\n") {
				entry = entry[paren+1:]
			}
		}
		desc, _ := strings.CutPrefix(entry, ": ")
		if desc == entry {
			desc = "" // no ": ", so no description
		}
		desc = strings.TrimSpace(desc)
		out[n] = claudeListed{Description: desc, Shortened: strings.HasSuffix(desc, "…") || unsure[i]}
	}
	return out
}

// listingEntry is where the entry for name starts in content, at or after from, or -1.
func listingEntry(content, name string, from int) int {
	head := "- " + name
	for from <= len(content) {
		i := strings.Index(content[from:], head)
		if i < 0 {
			return -1
		}
		at := from + i
		next := content[at+len(head):]
		if (at == 0 || content[at-1] == '\n') &&
			(next == "" || next[0] == '\n' || strings.HasPrefix(next, ": ") || strings.HasPrefix(next, " (")) {
			return at
		}
		from = at + 1
	}
	return -1
}

// readClaudeSkillListings merges the listings at the transcript's offsets, in order: a whole
// description beats a missing or shortened one, and among whole ones the later wins. A
// listing is judged again where it was written, as admits stands now: one saved in a
// checkout the team has since stopped collecting is left out.
func readClaudeSkillListings(path string, offsets []int64, admits func(cwd string) bool) map[string]claudeListed {
	out := map[string]claudeListed{}
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
		if json.Unmarshal(line, &rec) != nil || rec.Attachment.Type != "skill_listing" || !admits(rec.Cwd) {
			continue
		}
		for n, l := range parseSkillListing(rec.Attachment.Content, rec.Attachment.Names) {
			if had, ok := out[n]; !ok || l.whole() || !had.whole() {
				out[n] = l
			}
		}
	}
	return out
}
