package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
func captureClaudeSkills(e hookrun.Env, r *hookrun.Repo, sessionID, transcript string) {
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
	uses, next, err := readClaudeSkillUses(transcript, cur)
	if err != nil {
		e.Logf("skills: %v", err)
		return
	}
	var skills []hookrun.Skill
	if len(uses) > 0 {
		skills = claudeSkills(e, r, uses, readClaudeSkillListings(transcript, next.Listings))
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
// that failed. The listing's description is the skill's own when whole; else the skill's
// file gives it, composed as Claude Code composes the listing's.
func claudeSkills(e hookrun.Env, r *hookrun.Repo, uses []claudeSkillUse, listed map[string]claudeListed) []hookrun.Skill {
	var names []string
	dirs := map[string]string{}
	for _, u := range uses {
		if !slices.Contains(names, u.Name) {
			names = append(names, u.Name)
		}
		if dirs[u.Name] == "" && filepath.IsAbs(u.Dir) {
			dirs[u.Name] = u.Dir
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
		switch {
		case l.whole():
			s.Description = l.Description
		case dir != "":
			if fm, ok := hookrun.ReadSkillFrontmatter(filepath.Join(dir, claudeSkillFile)); ok {
				s.Description = strings.Join(slices.DeleteFunc([]string{fm.Description, fm.WhenToUse}, func(v string) bool { return v == "" }), " - ")
			}
		}
		out = append(out, s)
	}
	return out
}
