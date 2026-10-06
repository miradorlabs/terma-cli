package hookrun

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

const fundingStateDir = "funding"

// AgentsDir is the state directory's folder of agents' hook state, one folder per agent.
const AgentsDir = "agents"

// AgentStateDir is where agent's hook state of kind lives under the state directory.
func AgentStateDir(agent, kind string) string { return filepath.Join(AgentsDir, agent, kind) }

// SnapshotStateRetention is how long a snapshot or evidence hash outlives its last write;
// a live session rewrites one at least every QuotaHeartbeat.
const SnapshotStateRetention = 48 * time.Hour

// observationLockWait is how long an observation waits for the checkpoint before reporting a capture gap.
const (
	observationLockWait = time.Second
	observationLockPoll = 10 * time.Millisecond
)

// WriteState atomically replaces a cursor or checkpoint without fsync: one more durable
// than the unsynced spool would claim lost events were sent, and F_FULLFSYNC costs ~10 ms per call.
func WriteState(path string, data []byte) error {
	return config.WriteFileAtomicNoSync(path, data, 0o600)
}

// EvidenceID is the stable id for s, which makes a replay harmless.
func EvidenceID(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// QuotaHeartbeat is how often an unchanged snapshot is re-sent, so "no change" differs from "no capture".
const QuotaHeartbeat = 10 * time.Minute

// PruneState removes `<id>.json` files and abandoned temporary files older than before,
// then each lock that is past the cutoff, has no data file and is not held. A data file
// goes under its lock and only if still old, so a hook resuming it keeps it.
func PruneState(dir string, before time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var locks []string
	for _, ent := range entries {
		info, err := ent.Info()
		if err != nil || ent.IsDir() || !info.ModTime().Before(before) {
			continue
		}
		switch name := ent.Name(); {
		case strings.HasSuffix(name, ".json"):
			pruneData(filepath.Join(dir, name), before)
		case strings.HasPrefix(name, config.TempPrefix):
			_ = os.Remove(filepath.Join(dir, name))
		case strings.HasSuffix(name, ".json.lock"):
			locks = append(locks, filepath.Join(dir, name))
		}
	}
	for _, lock := range locks {
		if _, err := os.Lstat(lock); err != nil {
			continue // went with its data file
		}
		if _, err := os.Lstat(strings.TrimSuffix(lock, ".lock")); !os.IsNotExist(err) {
			continue
		}
		unlock, err := LockEvidence(lock)
		if err != nil {
			continue
		}
		flock.Remove(lock, unlock)
	}
}

// pruneData removes the data file at path, and its lock, if it is still older than before
// once its lock is had.
func pruneData(path string, before time.Time) {
	unlock, err := LockEvidence(path + ".lock")
	if err != nil {
		return
	}
	if info, err := os.Stat(path); err == nil && info.ModTime().Before(before) {
		_ = os.Remove(path)
	}
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		flock.Remove(path+".lock", unlock)
		return
	}
	unlock()
}

// StateDirs are the hook state directories the runtime itself keeps, beside each agent's.
var StateDirs = []string{fundingStateDir}

// Sweep ages out the hook state directories dirs, named under the state directory dir,
// and every private workspace store: the relay runs it, so state no hook revisits still goes.
func Sweep(dir string, now time.Time, dirs []string) {
	for _, d := range dirs {
		PruneState(filepath.Join(dir, d), now.Add(-spool.MaxAge))
	}
	workspaces := filepath.Join(dir, project.WorkspacesDir)
	stores, _ := os.ReadDir(workspaces)
	for _, d := range stores {
		if d.IsDir() {
			root := filepath.Join(workspaces, d.Name())
			session.Open(root).Retire(now.Add(-ManifestRetention))
			_ = os.Remove(root) // only once Retire emptied it
		}
	}
}
