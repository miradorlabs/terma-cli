package hookrun

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// State directory names are on developers' disks: renaming one orphans its files.
const (
	fundingStateDir = "funding"
)

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

// PruneState removes `<id>.json` files older than before, then each lock that is past the
// cutoff, has no data file and is not held.
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
			_ = os.Remove(filepath.Join(dir, name))
		case strings.HasSuffix(name, ".json.lock"):
			locks = append(locks, filepath.Join(dir, name))
		}
	}
	for _, lock := range locks {
		if _, err := os.Lstat(strings.TrimSuffix(lock, ".lock")); !os.IsNotExist(err) {
			continue
		}
		unlock, err := LockEvidence(lock)
		if err != nil {
			continue
		}
		_ = os.Remove(lock)
		unlock()
	}
}
