package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// State directories: which skills each session reported, and how far each transcript's skill
// uses were read.
var (
	claudeSkillStateDir  = hookrun.AgentStateDir(name, "skills")
	claudeSkillCursorDir = hookrun.AgentStateDir(name, "skill-cursors")
)

// captureClaudeSkills spools a terma.skill.definition for each skill the transcript shows
// the session used since the last read, when a turn stops or fails or the session ends. A
// definition is content of both kinds, so nothing is read while the team's policy withholds
// either.
func captureClaudeSkills(ctx context.Context, e hookrun.Env, r *hookrun.Repo, sessionID, transcript string) {
	if transcript == "" || e.Spool == nil || !session.ValidID(sessionID) || !e.SkillsCollected() {
		return
	}
	dir := filepath.Join(e.StateDir, claudeSkillCursorDir)
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
	var cur claudeSkillCursor
	b, readErr := os.ReadFile(path)
	if readErr == nil && json.Unmarshal(b, &cur) != nil {
		cur = claudeSkillCursor{}
	}
	// Each record is judged by where the session ran when it was written, once per directory.
	judged := map[string]bool{}
	admits := func(cwd string) bool {
		ok, seen := judged[cwd]
		if !seen {
			// With GIT_DIR set every directory reads as that one checkout, so only the
			// hook's own is judged; what was written anywhere else stays here.
			ok = cwd != "" && (os.Getenv("GIT_DIR") == "" || cwd == e.Cwd) && e.Admits(ctx, cwd)
			judged[cwd] = ok
		}
		return ok
	}
	uses, next, err := readClaudeSkillUses(transcript, cur, admits)
	if err != nil {
		e.Logf("skills: %v", err)
		return
	}
	var skills []hookrun.Skill
	if len(uses) > 0 {
		skills = claudeSkills(e, r, uses, readClaudeSkillListings(transcript, next.Listings, admits))
	}
	attrs := map[string]any{semconv.GenAIMainAgentNameKey: claudeTool, semconv.TermaEvidenceSourceKey: sourceClaudeTranscript}
	// Unsent skills stay past the stored cursor, for the next stop.
	if !hookrun.SpoolSkills(e, r, claudeSkillStateDir, sessionID, attrs, skills) {
		return
	}
	if b, err := json.Marshal(next); err == nil {
		if err := hookrun.WriteState(path, b); err != nil {
			e.Logf("skill cursor: %v", err)
		}
	}
	if os.IsNotExist(readErr) {
		hookrun.PruneState(dir, e.Time().Add(-spool.MaxAge))
	}
}

// claudeSkills is each skill used, once, with its description and where it is held. A name
// neither listed nor loaded from a directory is no skill: a built-in command, or a call
// that failed.
//
// Claude Code lists a skill whose frontmatter has no description with its body's first line,
// which must not leave. So where the skill's file is read, the file alone speaks: its
// description, composed with when_to_use as Claude Code composes the listing's, or none.
// Where no file is read (a built-in, a command file, which names no directory), the
// listing's goes only when whole and told apart from the loaded text's first line.
//
// Accepted risk: a skill with no file read whose loaded text is not in this transcript (one
// that runs in a subagent's own) sends no description; and telling a listing entry from a
// body's first line follows how Claude Code makes and escapes one today, so a build that
// changes it may send a command's first line. Upgrade path: Claude Code saying in its
// listing whether the author wrote the description.
func claudeSkills(e hookrun.Env, r *hookrun.Repo, uses []claudeSkillUse, listed map[string]claudeListed) []hookrun.Skill {
	var names []string
	dirs := map[string]string{}
	loaded := map[string]claudeSkillUse{}
	for _, u := range uses {
		if !slices.Contains(names, u.Name) {
			names = append(names, u.Name)
		}
		if dirs[u.Name] == "" && filepath.IsAbs(u.Dir) {
			dirs[u.Name] = u.Dir
		}
		if _, ok := loaded[u.Name]; !ok && u.loaded {
			loaded[u.Name] = u
		}
	}
	var out []hookrun.Skill
	for _, n := range names {
		l, inListing := listed[n]
		dir := dirs[n]
		if !inListing && dir == "" {
			continue
		}
		s := hookrun.Skill{}
		if dir != "" {
			s = claudeSkillSource(e, r, dir)
		}
		s.Name = n
		fm, inFile := hookrun.SkillFrontmatter{}, false
		if dir != "" {
			fm, inFile = hookrun.ReadSkillFrontmatter(filepath.Join(dir, claudeSkillFile))
		}
		u, read := loaded[n]
		switch {
		case inFile:
			if fm.Description != "" {
				s.Description = strings.Join(slices.DeleteFunc([]string{fm.Description, fm.WhenToUse}, func(v string) bool { return v == "" }), " - ")
			}
		case l.whole() && read && !bodyDerived(l.Description, u.opening, u.cut):
			s.Description = l.Description
		}
		out = append(out, s)
	}
	return out
}

// bodyDerived reports whether a listing's description may be the one Claude Code made from
// a body whose loaded text opens with opening (cut: Claude Code cut it there). The two are
// compared by their letters and digits, since Claude Code escapes and respaces what it lists:
// the description is the opening, alone or before the " - " and when_to_use Claude Code adds,
// or starts with a cut one. One holding `$` or "!`" may be the body's too: Claude Code fills
// arguments and runs commands in the loaded text, not the listing, so they cannot be compared.
func bodyDerived(description, opening string, cut bool) bool {
	if strings.Contains(description, "$") || strings.Contains(description, "!`") {
		return true
	}
	o := lettersAndDigits(opening)
	if o == "" {
		return false
	}
	if cut {
		return strings.HasPrefix(lettersAndDigits(description), o)
	}
	for part := description; ; {
		if lettersAndDigits(part) == o {
			return true
		}
		i := strings.LastIndex(part, " - ")
		if i < 0 {
			return false
		}
		part = part[:i]
	}
}

// lettersAndDigits is s's letters and digits, lowercased.
func lettersAndDigits(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}
