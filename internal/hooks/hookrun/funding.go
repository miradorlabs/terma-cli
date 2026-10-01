package hookrun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// evidenceState holds only a hash, never credential or provider file contents.
type evidenceState struct {
	Hash string    `json:"hash"`
	At   time.Time `json:"at"`
}

// CaptureFunding spools funding evidence for the session unless the same evidence was spooled within the heartbeat.
func (e Env) CaptureFunding(r *Repo, id, tool, name string, evidence FundingEvidence) {
	if e.Spool == nil || !session.ValidID(id) {
		return
	}
	attrs := evidence.Attrs
	if attrs == nil {
		attrs = map[string]any{}
	}
	attrs[AttrTool], attrs[AttrVersion] = tool, e.Version
	attrs[AttrEvidenceSource], attrs[AttrEvidenceStatus] = evidence.Source, evidence.Status
	attrs[AttrSchemaVersion] = 1
	if !evidence.SourceTime.IsZero() {
		attrs["source_time"] = evidence.SourceTime.UTC().Format(time.RFC3339Nano)
	}
	// Include project routing in the hash: a resumed session may move projects.
	attrs[AttrProjectID] = r.ProjectID
	r.StampWorktree(attrs)
	raw, err := json.Marshal(attrs)
	if err != nil {
		return
	}
	hash := sha256.Sum256(raw)
	key := sha256.Sum256([]byte(tool + "\x00" + name + "\x00" + id))
	dir, err := config.Dir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, fundingStateDir)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	path := filepath.Join(dir, hex.EncodeToString(key[:16])+".json")
	unlock, err := LockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var prev evidenceState
	fresh := false
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &prev)
	} else {
		fresh = os.IsNotExist(err)
	}
	next := evidenceState{Hash: hex.EncodeToString(hash[:]), At: e.Time()}
	if prev.Hash == next.Hash && !next.At.Before(prev.At) && next.At.Sub(prev.At) < QuotaHeartbeat {
		return
	}
	ev := spool.Event{Time: e.Time(), Name: name, SessionID: id, Repo: r.Name, Attrs: attrs}
	if e.Spool.Append(ev) != nil {
		return
	} // Retry a failed append at the next hook.
	data, _ := json.Marshal(next)
	_ = WriteState(path, data)
	if fresh {
		PruneState(dir, next.At.Add(-SnapshotStateRetention))
	}
}
