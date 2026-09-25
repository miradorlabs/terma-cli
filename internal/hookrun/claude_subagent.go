package hookrun

import (
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Claude's internal forks mint agent ids and fire SubagentStop without ever
// dispatching SubagentStart or being launched by Agent/Task. Their agent_type can
// inherit the session's --agent, so neither an id nor a type proves delegation.
// Keep launch evidence across hook processes and spool flushes instead.
//
// Each (session, agent) has its own marker, written whole with no read/modify/write
// cycle or shared index. Concurrent launches cannot overwrite one another. The
// repository is not part of the key: a subagent can run in another worktree.
func claudeSubagentPath(sessionID, agentID string) (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, claudeSubagentDir, evidenceID(sessionID+"\x00"+agentID)+".json"), nil
}

func (e Env) rememberClaudeSubagent(sessionID, agentID string) {
	if e.Spool == nil {
		return
	}
	path, err := claudeSubagentPath(sessionID, agentID)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o700)
	}
	if err == nil {
		err = writeState(path, []byte("{}\n"))
	}
	if err != nil {
		e.logf("record Claude subagent launch: %v", err)
	}
}

func (e Env) knownClaudeSubagent(sessionID, agentID string) bool {
	path, err := claudeSubagentPath(sessionID, agentID)
	if err != nil {
		return false
	}
	info, err := os.Lstat(path)
	// Keep the marker after a stop: hooks can keep an agent running or it can be
	// resumed. SessionStart prunes old markers; enforce the same age on reads.
	return err == nil && info.Mode().IsRegular() && !info.ModTime().Before(e.now().Add(-spool.MaxAge))
}

func (e Env) pruneClaudeSubagents() {
	if dir, err := config.Dir(); err == nil {
		pruneQuotaState(filepath.Join(dir, claudeSubagentDir), e.now().Add(-spool.MaxAge))
	}
}
