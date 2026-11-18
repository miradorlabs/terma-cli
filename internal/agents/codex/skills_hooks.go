package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// State directories: which skills each session reported, and how far each rollout's $name
// skills were read.
var (
	codexSkillStateDir  = hookrun.AgentStateDir(name, "skills")
	codexSkillCursorDir = hookrun.AgentStateDir(name, "skill-cursors")
)

// skillAttrs are the agent's own attributes on a skill's definition.
func skillAttrs(source string) map[string]any {
	return map[string]any{semconv.GenAIMainAgentNameKey: codexTool, semconv.TermaEvidenceSourceKey: source}
}

// captureCodexSkillReads spools a terma.skill.definition for each skill a shell call printed
// the SKILL.md of: how the model loads a skill. A definition is content of both kinds, so
// nothing is read while the team's policy withholds either.
func captureCodexSkillReads(e hookrun.Env, r *hookrun.Repo, in *codexHookInput) {
	if in.ToolName != codexShellTool || !session.ValidID(in.SessionID) || !e.SkillsCollected() {
		return
	}
	var input struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(in.ToolInput, &input) != nil {
		return
	}
	var skills []hookrun.Skill
	for _, p := range shellSkillReads(input.Command, e.Cwd) {
		if s, ok := codexSkill(e, r, p, ""); ok {
			skills = append(skills, s)
		}
	}
	// Accepted risk: a read that cannot be spooled now (the session's state held past the
	// wait, the spool refusing) is not reported, since no cursor returns to a hook's payload;
	// the skill's next read reports it. Upgrade path: park it, as a sandboxed hook's events are.
	hookrun.SpoolSkills(e, r, codexSkillStateDir, in.SessionID, skillAttrs(sourceCodexHook), skills)
}

// captureCodexSkillMentions spools a terma.skill.definition for each $name skill the rollout
// records since the last capture, in the turns the team collects.
func captureCodexSkillMentions(ctx context.Context, e hookrun.Env, r *hookrun.Repo, in *codexHookInput) {
	if e.Spool == nil || !session.ValidID(in.SessionID) || !e.SkillsCollected() {
		return
	}
	dir := filepath.Join(e.StateDir, codexSkillCursorDir)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	rollout := codexRolloutID(in)
	path := filepath.Join(dir, hookrun.EvidenceID(rollout)+".json")
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var cursor skillCursor
	b, readErr := os.ReadFile(path)
	if readErr == nil && json.Unmarshal(b, &cursor) != nil {
		cursor = skillCursor{}
	}
	ctx, cancel := context.WithTimeout(ctx, codexCaptureTimeout)
	defer cancel()
	turnAdmits := turnAdmission(ctx, e)
	// With GIT_DIR set every directory reads as that one checkout, so only a turn that ran in
	// the hook's own is judged; one that ran anywhere else stays here.
	gitDir := os.Getenv("GIT_DIR") != ""
	admits := func(cwd string) bool { return (!gitDir || cwd == e.Cwd) && turnAdmits(cwd) }
	// A resumed thread's earlier turns may have run in another collected checkout: a skill is
	// judged against the checkout its own turn ran in, where a repository's skills are found.
	// Its definition is still placed with this hook's checkout, which the session is claimed for.
	turnRepos := map[string]*hookrun.Repo{e.Cwd: r}
	turnRepo := func(cwd string) *hookrun.Repo {
		if _, seen := turnRepos[cwd]; !seen {
			root, _, ok := gitx.LocateFS(cwd)
			if !ok {
				root = cwd
			}
			turnRepos[cwd] = &hookrun.Repo{Root: root}
		}
		return turnRepos[cwd]
	}
	for {
		var skills []hookrun.Skill
		next, status, err := readRolloutSkills(ctx, rollout, in.TranscriptPath, cursor, admits, func(name, p, cwd string) error {
			if s, ok := codexSkill(e, turnRepo(cwd), p, name); ok {
				skills = append(skills, s)
			}
			return nil
		})
		if err != nil {
			e.Logf("codex skills (%s): %v", status, err)
		}
		// Unsent skills stay past the stored cursor, for the next capture.
		if next == cursor || !hookrun.SpoolSkills(e, r, codexSkillStateDir, in.SessionID, skillAttrs(sourceCodexRollout), skills) {
			break
		}
		if b, err := json.Marshal(next); err == nil {
			if err := hookrun.WriteState(path, b); err != nil {
				e.Logf("skill cursor: %v", err)
			}
		}
		cursor = next
		if status != "backlog" || err != nil {
			break
		}
	}
	if os.IsNotExist(readErr) {
		hookrun.PruneState(dir, e.Time().Add(-spool.MaxAge))
	}
}
