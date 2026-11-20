package claude

import (
	"context"
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
	// Each use is judged by where the session ran when it was written, once per directory.
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
// that failed.
//
// A description leaves only when it is read from the frontmatter of the skill's own file:
// its description, joined with when_to_use as Claude Code joins them. A file with none, no
// frontmatter or one that cannot be read gives none; Claude Code would describe that skill by
// its body's first line.
func claudeSkills(e hookrun.Env, r *hookrun.Repo, uses []claudeSkillUse, listed map[string]bool) []hookrun.Skill {
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
		dir := dirs[n]
		if !listed[n] && dir == "" {
			continue
		}
		s := hookrun.Skill{}
		// Accepted risk: built-in skills, command files, plugin commands and MCP-served
		// skills name no directory, so they are reported with no description: Claude Code's
		// listing, the only place theirs is written, also holds lines it took from bodies,
		// and nothing in the transcript tells the two apart. Upgrade path: built-in
		// descriptions are the same for every user and can come from a table on the platform.
		if dir != "" {
			s = claudeSkillSource(e, r, dir)
			if fm, ok := hookrun.ReadSkillFrontmatter(filepath.Join(dir, claudeSkillFile)); ok && fm.Description != "" {
				s.Description = strings.Join(slices.DeleteFunc([]string{fm.Description, fm.WhenToUse}, func(v string) bool { return v == "" }), " - ")
			}
		}
		s.Name = n
		out = append(out, s)
	}
	return out
}
