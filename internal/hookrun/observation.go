package hookrun

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// observation is one snapshot bound for the spool, with the identity the write-ahead
// checkpoint needs to order it. Every hooks-only harness (Cursor, Antigravity) records
// through this; the attributes are the harness's, the ordering and replay identity are
// shared.
type observation struct {
	// tool names the harness ("cursor"); it also seeds the observation id.
	tool string
	// source is the evidence_source attribute on a capture-gap event.
	source string
	// stateDir is the checkpoint directory under the config dir, per harness so a
	// harness's stream survives another's being introduced.
	stateDir  string
	sessionID string
	hook      string
	// turnID is the harness's turn identifier when it has one; it travels on the
	// capture-gap event so a lost observation can be placed.
	turnID string
	attrs  map[string]any
}

// observationState is a write-ahead checkpoint. A crash between spool append and
// checkpoint acknowledgement replays Pending with the same observation ID. Ordering is
// local receipt order, not an invented provider timestamp/order.
type observationState struct {
	Stream   string       `json:"stream"`
	Sequence uint64       `json:"sequence"`
	LastHash string       `json:"last_hash"`
	At       time.Time    `json:"at"`
	Pending  *spool.Event `json:"pending,omitempty"`
}

// captureObservation appends o to the spool with a durable per-stream sequence,
// suppressing adjacent identical snapshots for up to the heartbeat interval.
func (e Env) captureObservation(ctx context.Context, r *repo, o observation) {
	if e.Spool == nil {
		return
	}
	attrs := o.attrs
	if attrs == nil {
		attrs = map[string]any{}
	}
	attrs[attrVersion], attrs[AttrProjectID] = e.Version, r.projectID
	raw, _ := json.Marshal(attrs)
	hash := evidenceID(string(raw))
	dir, err := config.Dir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, o.stateDir)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	path := filepath.Join(dir, evidenceID(o.sessionID+"\x00"+r.root)+".json")
	// Unlike a redraw, distinct hooks cannot simply be discarded when another hook
	// holds the lock. Wait briefly, bounded by the hook's deadline.
	ctx, cancel := context.WithTimeout(ctx, observationLockWait)
	defer cancel()
	var unlock func()
	for {
		unlock, err = lockEvidence(path + ".lock")
		if err == nil {
			break
		}
		if !isLockBusy(err) {
			e.logf("%s capture lock: %v", o.tool, err)
			return
		}
		select {
		case <-ctx.Done():
			gap := map[string]any{attrTool: o.tool, attrEvidenceSource: o.source, attrEvidenceStatus: "lock_timeout", attrHookEvent: o.hook}
			if o.turnID != "" {
				gap[attrTurnID] = o.turnID
			}
			e.emitFor(r, spool.Event{Name: EventSessionCapture, SessionID: o.sessionID, Repo: repoName(r.root), Attrs: gap})
			return
		case <-time.After(observationLockPoll):
		}
	}
	defer unlock()
	var state observationState
	b, err := readCheckpoint(path)
	fresh := os.IsNotExist(err)
	if err != nil && !fresh {
		e.logf("%s checkpoint read: %v", o.tool, err)
		return
	}
	if !fresh && (len(b) > 64<<10 || json.Unmarshal(b, &state) != nil || state.Stream == "") {
		// A new stream makes the loss of the old ordering boundary visible.
		state = observationState{}
		attrs["capture_gap"] = "invalid_checkpoint"
	}
	if state.Stream == "" {
		state.Stream = rand.Text()
	}
	write := func() error {
		b, err := json.Marshal(state)
		if err != nil {
			return err
		}
		return writeState(path, b)
	}
	if state.Pending != nil {
		if err = e.Spool.Append(*state.Pending); err != nil {
			e.logf("%s pending append: %v", o.tool, err)
			return
		}
		state.Pending = nil
		if err = write(); err != nil {
			e.logf("%s checkpoint: %v", o.tool, err)
			return
		}
	}
	// Suppress only adjacent identical snapshots. Changed hook, turn, model, account,
	// loop count, missingness or values always retains a new position.
	if state.LastHash == hash && !e.now().Before(state.At) && e.now().Sub(state.At) < quotaHeartbeat {
		return
	}
	state.Sequence++
	state.LastHash, state.At = hash, e.now()
	attrs["source_stream"], attrs["observation_sequence"] = state.Stream, state.Sequence
	attrs["observation_id"] = evidenceID(fmt.Sprintf("%s\x00%s\x00%d", o.tool, state.Stream, state.Sequence))
	state.Pending = &spool.Event{Time: e.now(), Name: EventSessionObservation, SessionID: o.sessionID, Repo: repoName(r.root), Attrs: attrs}
	if err = write(); err != nil {
		e.logf("%s checkpoint: %v", o.tool, err)
		return
	}
	if err = e.Spool.Append(*state.Pending); err != nil {
		e.logf("%s observation append: %v", o.tool, err)
		return
	}
	state.Pending = nil
	if err = write(); err != nil {
		e.logf("%s checkpoint: %v", o.tool, err)
	}
	if fresh {
		pruneQuotaState(dir, e.now().Add(-spool.MaxAge))
	}
}

func readCheckpoint(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("checkpoint is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, (64<<10)+1))
}
