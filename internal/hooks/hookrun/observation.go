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

// Observation is one snapshot bound for the spool, with the identity the write-ahead
// checkpoint needs to order it. An agent that reports only through its hooks records
// through this; the attributes are the agent's, the ordering and replay identity are
// shared.
type Observation struct {
	// Tool is the agent's label; it also seeds the observation id.
	Tool string
	// Source is the evidence_source attribute on a capture-gap event.
	Source string
	// StateDir is the checkpoint directory under the config dir, per harness so a
	// harness's stream survives another's being introduced.
	StateDir  string
	SessionID string
	Hook      string
	// TurnID is the harness's turn identifier when it has one; it travels on the
	// capture-gap event so a lost observation can be placed.
	TurnID string
	Attrs  map[string]any
}

// ObservationState is a write-ahead checkpoint. A crash between spool append and
// checkpoint acknowledgement replays Pending with the same observation ID. Ordering is
// local receipt order, not an invented provider timestamp/order.
type ObservationState struct {
	Stream   string       `json:"stream"`
	Sequence uint64       `json:"sequence"`
	LastHash string       `json:"last_hash"`
	At       time.Time    `json:"at"`
	Pending  *spool.Event `json:"pending,omitempty"`
}

// CaptureObservation appends o to the spool with a durable per-stream sequence,
// suppressing adjacent identical snapshots for up to the heartbeat interval.
func (e Env) CaptureObservation(ctx context.Context, r *Repo, o Observation) {
	if e.Spool == nil {
		return
	}
	attrs := o.Attrs
	if attrs == nil {
		attrs = map[string]any{}
	}
	attrs[AttrVersion], attrs[AttrProjectID] = e.Version, r.ProjectID
	r.StampWorktree(attrs)
	raw, _ := json.Marshal(attrs)
	hash := EvidenceID(string(raw))
	dir, err := config.Dir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, o.StateDir)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	path := filepath.Join(dir, EvidenceID(o.SessionID+"\x00"+r.Root)+".json")
	// Unlike a redraw, distinct hooks cannot simply be discarded when another hook
	// holds the lock. Wait briefly, bounded by the hook's deadline.
	ctx, cancel := context.WithTimeout(ctx, observationLockWait)
	defer cancel()
	var unlock func()
	for {
		unlock, err = LockEvidence(path + ".lock")
		if err == nil {
			break
		}
		if !isLockBusy(err) {
			e.Logf("%s capture lock: %v", o.Tool, err)
			return
		}
		select {
		case <-ctx.Done():
			gap := map[string]any{AttrTool: o.Tool, AttrEvidenceSource: o.Source, AttrEvidenceStatus: "lock_timeout", AttrHookEvent: o.Hook}
			if o.TurnID != "" {
				gap[AttrTurnID] = o.TurnID
			}
			e.EmitFor(r, spool.Event{Name: EventSessionCapture, SessionID: o.SessionID, Repo: r.Name, Attrs: gap})
			return
		case <-time.After(observationLockPoll):
		}
	}
	defer unlock()
	var state ObservationState
	b, err := readCheckpoint(path)
	fresh := os.IsNotExist(err)
	if err != nil && !fresh {
		e.Logf("%s checkpoint read: %v", o.Tool, err)
		return
	}
	if !fresh && (len(b) > 64<<10 || json.Unmarshal(b, &state) != nil || state.Stream == "") {
		// A new stream makes the loss of the old ordering boundary visible.
		state = ObservationState{}
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
		return WriteState(path, b)
	}
	if state.Pending != nil {
		if err = e.Spool.Append(*state.Pending); err != nil {
			e.Logf("%s pending append: %v", o.Tool, err)
			return
		}
		state.Pending = nil
		if err = write(); err != nil {
			e.Logf("%s checkpoint: %v", o.Tool, err)
			return
		}
	}
	// Suppress only adjacent identical snapshots. Changed hook, turn, model, account,
	// loop count, missingness or values always retains a new position.
	if state.LastHash == hash && !e.Time().Before(state.At) && e.Time().Sub(state.At) < QuotaHeartbeat {
		return
	}
	state.Sequence++
	state.LastHash, state.At = hash, e.Time()
	attrs["source_stream"], attrs["observation_sequence"] = state.Stream, state.Sequence
	attrs["observation_id"] = EvidenceID(fmt.Sprintf("%s\x00%s\x00%d", o.Tool, state.Stream, state.Sequence))
	state.Pending = &spool.Event{Time: e.Time(), Name: EventSessionObservation, SessionID: o.SessionID, Repo: r.Name, Attrs: attrs}
	if err = write(); err != nil {
		e.Logf("%s checkpoint: %v", o.Tool, err)
		return
	}
	if err = e.Spool.Append(*state.Pending); err != nil {
		e.Logf("%s observation append: %v", o.Tool, err)
		return
	}
	state.Pending = nil
	if err = write(); err != nil {
		e.Logf("%s checkpoint: %v", o.Tool, err)
	}
	if fresh {
		PruneState(dir, e.Time().Add(-spool.MaxAge))
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
