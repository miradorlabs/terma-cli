package hookrun

import (
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// State directories under the config dir. Each holds one small JSON file per session,
// named by a hash, with a `.lock` beside it; pruneQuotaState ages all of them out. The
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
	codexToolStartDir     = "codex-tool-starts"
	// cursorObservationDir and antigravityObservationDir are the observation
	// checkpoints, per harness so one harness's stream survives another's arrival.
	cursorObservationDir      = "cursor-observations"
	antigravityObservationDir = "antigravity-observations"
	// antigravityTurnDir holds one record per conversation, beside the observation
	// checkpoints: the turn agy is in (see antigravity_turn.go).
	antigravityTurnDir = "antigravity-turns"
)

// snapshotStateRetention is how long a status line snapshot or a funding evidence hash
// outlives its last write. Both are rewritten whenever a live session spools one, at
// least every quotaHeartbeat, so two idle days means the session is over. The cursors
// and checkpoints in the other directories are kept for spool.MaxAge, not for this.
const snapshotStateRetention = 48 * time.Hour

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

// writeState replaces one of the state files above: a cursor, a checkpoint, the hash of
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
func writeState(path string, data []byte) error {
	return config.WriteFileAtomicNoSync(path, data, 0o600)
}
