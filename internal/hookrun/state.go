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

// State directories under the config dir. Each holds small files named by a hash,
// with a `.lock` beside mutable checkpoints; pruneState ages them out. The
// names are on developers' disks already: renaming one orphans its files and restarts
// every sequence and cursor kept there.
const (
	// statusLineStateDir is the last quota snapshot each Claude Code session spooled.
	statusLineStateDir = "statusline"
	// fundingStateDir is the hash of the last funding evidence spooled per session.
	fundingStateDir = "funding"
	// codexFundingCursorDir and codexReplyCursorDir are how far into a Codex rollout
	// the quota capture and the reply capture have read.
	codexFundingCursorDir = "funding-cursors"
	codexReplyCursorDir   = "reply-cursors"
	codexDesktopCursorDir = "desktop-cursors"
	// codexTitleStateDir is when each Codex thread's name that was last spooled was written.
	codexTitleStateDir = "codex-titles"
	codexToolStartDir  = "codex-tool-starts"
	// cursorObservationDir and antigravityObservationDir are the observation
	// checkpoints, per harness so one harness's stream survives another's arrival.
	cursorObservationDir      = "cursor-observations"
	antigravityObservationDir = "antigravity-observations"
	// antigravityTurnDir holds one record per conversation, beside the observation
	// checkpoints: the turn agy is in (see antigravity_turn.go).
	antigravityTurnDir = "antigravity-turns"
	// claudeSubagentDir holds launch evidence per (session, agent), so internal
	// Claude forks' orphan stop hooks cannot create delegated runs.
	claudeSubagentDir = "claude-subagents"
)

// SnapshotStateRetention is how long a status line snapshot or a funding evidence hash
// outlives its last write. Both are rewritten whenever a live session spools one, at
// least every quotaHeartbeat, so two idle days means the session is over. The cursors
// and checkpoints in the other directories are kept for spool.MaxAge, not for this.
const SnapshotStateRetention = 48 * time.Hour

// codexCaptureTimeout bounds each of a Codex hook's rollout captures, quota and then
// replies. Stop is synchronous with a three-second timeout in the committed hooks file,
// and both captures have to finish inside it.
const codexCaptureTimeout = time.Second

// observationLockWait is how long an observation waits for another hook of the same
// session to release the checkpoint before it reports a capture gap, and
// observationLockPoll is how often it looks.
const (
	observationLockWait = time.Second
	observationLockPoll = 10 * time.Millisecond
)

// WriteState replaces one of the state files above: a cursor, a checkpoint, the hash of
// the last thing spooled. Atomic, so another hook never reads half of one — and not
// fsynced, on purpose.
//
// Each of these says how far capture has got, and what it has got is in the spool,
// which Append does not sync either. A cursor more durable than the events behind it
// fails the wrong way round: after a power cut it would say "already sent" about
// something that never reached the disk, and that evidence would be gone. One that is
// merely as durable as the spool falls behind instead and the hook replays, which is
// what the stable observation ids are for — a duplicate is dropped downstream, a gap
// is not recoverable. The sync it skips is F_FULLFSYNC on macOS: about ten
// milliseconds against a fifth of one, on every tool call, inside the agent's turn.
func WriteState(path string, data []byte) error {
	return config.WriteFileAtomicNoSync(path, data, 0o600)
}

// EvidenceID is the stable id for s: a state file's name, an observation's id. The
// same input is the same id on every run, which is what makes a replay harmless.
func EvidenceID(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// QuotaHeartbeat is how often an unchanged snapshot is re-sent, so the backend
// can tell "no change" from "no status line".
const QuotaHeartbeat = 10 * time.Minute

// PruneState ages out a state directory: every `<id>.json` last written before the
// cutoff, and then the `<id>.json.lock` beside it. The locks used to be left behind —
// one per session, for ever — because only the data files were matched.
//
// A lock goes only when all three hold: it is past the cutoff itself, its data file is
// gone, and nothing holds it (the prune takes it before unlinking). A lock's mtime is
// its creation, so a session that outlives the cutoff keeps its lock through its data
// file, which every write refreshes.
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
