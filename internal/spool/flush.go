package spool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// FlushOptions tunes one flush.
type FlushOptions struct {
	// Timeout bounds the entire pass (DefaultFlushTimeout when nonpositive).
	Timeout time.Duration
	// Batch is events per send (DefaultBatch when zero).
	Batch int
	// Force ignores the failure backoff.
	Force bool
	// MinInterval skips the flush when one completed more recently (`terma spool flush --min-interval`).
	MinInterval time.Duration
	Now         time.Time
}

// SkipReason says why a flush did nothing without failing.
type SkipReason string

const (
	// SkipBackoff means a previous delivery failed and its backoff window is open.
	SkipBackoff SkipReason = "backing off after a failed delivery"
	// SkipMinInterval means a flush completed more recently than the caller's throttle.
	SkipMinInterval SkipReason = "a flush completed within the throttle interval"
)

// Result summarizes a flush; its counts are disjoint, and only Expired, Pruned and
// Dropped are loss.
type Result struct {
	Sent int
	// Held counts events handed back for later; still queued.
	Held int
	// Failed counts events a PartialDelivery left undelivered; still queued.
	Failed int
	// Expired counts events older than MaxAge.
	Expired int
	// Pruned counts events discarded to keep the spool under MaxBytes.
	Pruned int
	// Dropped counts lines that would not decode, such as a killed hook's torn write.
	Dropped int
	Skipped bool
	Reason  SkipReason
	Err     error
}

// add folds one read's losses into the pass; Sent and Held are counted at delivery.
func (r *Result) add(b batch) {
	r.Expired += b.Expired
	r.Pruned += b.Pruned
	r.Dropped += b.Dropped
}

// Flush delivers queued events, trimming what was sent; a failed send opens a
// spool-wide backoff (30s..1h), and a PartialDelivery is acknowledged and the pass continues.
func (s *Spool) Flush(ctx context.Context, sender Sender, opts FlushOptions) Result {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultFlushTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Only flushers take this lock, so hooks can append during a send.
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, deliveryLockFile))
	if err != nil {
		return Result{Err: err}
	}
	defer unlock()

	// Check backoff after the lock: the previous flusher may have failed meanwhile.
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	if !opts.Force {
		if next := s.NextAttempt(); !next.IsZero() && now.Before(next) {
			return Result{Skipped: true, Reason: SkipBackoff}
		}
	}
	if opts.MinInterval > 0 {
		if last := s.LastFlush(); !last.IsZero() && now.Sub(last) < opts.MinInterval {
			return Result{Skipped: true, Reason: SkipMinInterval}
		}
	}
	batch := opts.Batch
	if batch <= 0 {
		batch = DefaultBatch
	}

	var res Result
	// Partial failures end the pass as a failure without opening the spool-wide window.
	var failures []error
	stop := func(err error) Result {
		res.Err = errors.Join(append(failures, err)...)
		return res
	}
	// Consume only the bytes present at the start; requeued and new events wait for the next pass.
	budget := int64(-1)
	for {
		if err := ctx.Err(); err != nil {
			return stop(err)
		}
		batchResult, err := s.readFlushBatch(ctx, batch, budget, now)
		res.add(batchResult)
		if err != nil {
			return stop(err)
		}
		if budget < 0 {
			budget = batchResult.Size
		}
		var held, undelivered []Event
		if len(batchResult.Events) > 0 {
			held, err = sender.Send(ctx, batchResult.Events)
			if partial, ok := errors.AsType[*PartialDelivery](err); ok {
				undelivered = partial.Undelivered
				if partial.Err != nil {
					failures = append(failures, partial)
				}
			} else if err != nil {
				failureTime := opts.Now
				if failureTime.IsZero() {
					failureTime = time.Now()
				}
				s.recordFailure(failureTime)
				res.Err = errors.Join(append(failures, err)...)
				return res
			}
		}
		// One atomic rewrite acknowledges the batch and requeues; if cancelled, the batch replays.
		requeue := slices.Concat(held, undelivered)
		if batchResult.Consumed > 0 {
			if err := s.ackBatch(ctx, batchResult.Consumed, batchResult.Prefix, requeue); err != nil {
				return stop(err)
			}
		}
		res.Sent += len(batchResult.Events) - len(requeue)
		res.Held += len(held)
		res.Failed += len(undelivered)
		budget -= batchResult.Consumed
		if len(batchResult.Events) < batch || budget <= 0 {
			// The pass reached every destination, so a spool-wide window has nothing to protect.
			s.clearBackoff()
			if len(failures) > 0 {
				res.Err = errors.Join(failures...)
				return res
			}
			s.recordFlush(now)
			return res
		}
	}
}

// batch is one read from the head of the queue, with every removed event counted once.
type batch struct {
	Events []Event
	// Consumed is the bytes an acknowledgement removes, skipped lines included.
	Consumed int64
	// Size is the file size the read saw, bounding what this pass may send.
	Size int64
	// Prefix hashes the consumed bytes, so the acknowledgement can refuse a changed queue.
	Prefix  [32]byte
	Expired int
	Pruned  int
	Dropped int
}

// readFlushBatch holds the append lock only while reading; it prunes only before
// the pass's initial byte budget.
func (s *Spool) readFlushBatch(ctx context.Context, n int, budget int64, now time.Time) (batch, error) {
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, lockFile))
	if err != nil {
		return batch{}, err
	}
	defer unlock()
	pruned := 0
	if budget < 0 {
		if pruned, err = s.prune(); err != nil {
			return batch{}, err
		}
	}
	b, err := s.readBatch(n, max(budget, 0), now)
	b.Pruned = pruned // readBatch counts what it read, not what pruning removed
	return b, err
}

// readBatch decodes up to n events from the head, no further than a positive limit;
// undecodable and (when now is set) expired lines are counted and leave with the batch.
func (s *Spool) readBatch(n int, limit int64, now time.Time) (batch, error) {
	var b batch
	f, err := os.Open(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return b, nil
	}
	if err != nil {
		return batch{}, err
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return batch{}, err
	}
	b.Size = int64(len(data))
	for len(b.Events) < n {
		if limit > 0 && b.Consumed >= limit {
			break // the rest was appended after this pass began
		}
		idx := bytes.IndexByte(data[b.Consumed:], '\n')
		if idx < 0 || (limit > 0 && b.Consumed+int64(idx)+1 > limit) {
			break // leave an incomplete line (including one completed after the snapshot)
		}
		line := data[b.Consumed : b.Consumed+int64(idx)]
		b.Consumed += int64(idx) + 1
		var e Event
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if err := json.Unmarshal(line, &e); err != nil {
			b.Dropped++
			continue
		}
		if !now.IsZero() && !e.Time.IsZero() && now.Sub(e.Time) > MaxAge {
			b.Expired++
			continue
		}
		b.Events = append(b.Events, e)
	}
	b.Prefix = sha256.Sum256(data[:b.Consumed])
	return b, nil
}

// ackBatch removes a delivered prefix and requeues held events in one rename,
// keeping whatever was appended meanwhile.
func (s *Spool) ackBatch(ctx context.Context, consumed int64, prefix [32]byte, held []Event) error {
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, lockFile))
	if err != nil {
		return err
	}
	defer unlock()
	data, err := os.ReadFile(s.path())
	if err != nil {
		return err
	}
	// A flusher that does not take delivery.lock may have trimmed this prefix meanwhile.
	if consumed > int64(len(data)) || sha256.Sum256(data[:consumed]) != prefix {
		return errors.New("spool changed during delivery")
	}
	tail := bytes.Clone(data[consumed:])
	for _, e := range held {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		tail = append(tail, line...)
		tail = append(tail, '\n')
	}
	return config.WriteFileAtomicNoSync(s.path(), tail, fileMode)
}

// prune bounds disk usage before a flush snapshot. Peek never mutates the queue:
// changing its prefix while a send is in flight would invalidate the acknowledgement.
func (s *Spool) prune() (int, error) {
	info, err := os.Stat(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if info.Size() <= MaxBytes {
		return 0, nil
	}
	data, err := os.ReadFile(s.path())
	if err != nil {
		return 0, err
	}
	cut := len(data) - MaxBytes/2
	if idx := bytes.IndexByte(data[cut:], '\n'); idx >= 0 {
		cut += idx + 1
	} else {
		cut = len(data)
	}
	if err := config.WriteFileAtomicNoSync(s.path(), data[cut:], fileMode); err != nil {
		return 0, err
	}
	return bytes.Count(data[:cut], []byte{'\n'}), nil
}
