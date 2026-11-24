package hookrun

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A subagent either runs inside its parent's session, stamped with terma.agent.id, or is a
// session of its own naming terma.session.parent.id; neither carries token usage.

// AgentAttrs stamps the agent facet when the payload names an agent id; a type without an
// id stamps nothing, since an agent may send its type on every hook.
func AgentAttrs(attrs map[string]any, id, kind string) map[string]any {
	if !session.ValidID(id) {
		return attrs
	}
	attrs[semconv.TermaAgentIDKey] = id
	if ShortLabel(kind) {
		attrs[semconv.GenAIAgentNameKey] = kind
	}
	return attrs
}

// ShortLabel reports whether v is present, at most 128 bytes, and on one line.
func ShortLabel(v string) bool {
	return v != "" && len(v) <= 128 && !strings.ContainsAny(v, "\r\n")
}

// SubagentEvidencePath is the marker that sessionID launched agentID, in the agent's state
// directory dir under the state directory stateDir. One marker per (session, agent), written
// whole, so concurrent launches never overwrite one another; the repository is not in the
// key, since a subagent can run in another worktree.
func SubagentEvidencePath(stateDir, dir, sessionID, agentID string) string {
	return filepath.Join(stateDir, dir, EvidenceID(sessionID+"\x00"+agentID)+".json")
}

// RememberSubagent records that sessionID launched agentID, in the agent's state directory
// dir. A hook with no spool records nothing, since it could send nothing the launch admits.
func (e Env) RememberSubagent(dir, sessionID, agentID string) {
	if e.Spool == nil {
		return
	}
	path := SubagentEvidencePath(e.StateDir, dir, sessionID, agentID)
	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		err = WriteState(path, []byte("{}\n"))
	}
	if err != nil {
		e.Logf("record subagent launch: %v", err)
	}
}

// KnownSubagent reports whether RememberSubagent recorded that sessionID launched agentID,
// within spool.MaxAge. The marker is kept after a stop, since an agent can be resumed.
func (e Env) KnownSubagent(dir, sessionID, agentID string) bool {
	info, err := os.Lstat(SubagentEvidencePath(e.StateDir, dir, sessionID, agentID))
	return err == nil && info.Mode().IsRegular() && !info.ModTime().Before(e.Time().Add(-spool.MaxAge))
}

// PruneSubagents ages the launch markers in the agent's state directory dir out like the spool.
func (e Env) PruneSubagents(dir string) {
	PruneState(filepath.Join(e.StateDir, dir), e.Time().Add(-spool.MaxAge))
}
