package desktoprelay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/project"
)

const (
	maxQueueBytes = 64 << 20
	maxPayload    = 20 << 20
	queueMaxAge   = 14 * 24 * time.Hour
)

type queueItem struct {
	ProjectID string    `json:"project_id"`
	Payload   []byte    `json:"payload"`
	AddedAt   time.Time `json:"added_at"`
}

// Queue durably holds project-separated OTLP requests until the ingest host accepts
// them. A server key is looked up only while sending and never stored with a batch.
type Queue struct {
	dir string
}

// OpenQueue prepares the private directory for desktop relay batches.
func OpenQueue() (*Queue, error) {
	base, err := config.Dir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, "desktop-relay", "queue")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Queue{dir: dir}, nil
}

// Enqueue acknowledges a Codex export only after its identifiable records have
// landed in the private local queue. A full queue returns an error so Codex can retry.
func (q *Queue) Enqueue(projectID string, payload []byte) error {
	return q.EnqueueMany(map[string][]byte{projectID: payload})
}

// EnqueueMany stages every project portion of one OTLP request under one size
// check. If a local write fails, the portions written in this call are removed
// before the receiver asks Codex to retry the request.
func (q *Queue) EnqueueMany(batches map[string][]byte) error {
	items := make(map[string][]byte, len(batches))
	var added int64
	for projectID, payload := range batches {
		if !project.ValidID(projectID) || len(payload) == 0 || len(payload) > maxPayload {
			return errors.New("invalid desktop relay batch")
		}
		item, err := json.Marshal(queueItem{ProjectID: projectID, Payload: payload, AddedAt: time.Now().UTC()})
		if err != nil {
			return err
		}
		items[projectID] = item
		added += int64(len(item))
	}
	if added > maxQueueBytes {
		return errors.New("desktop relay batch exceeds queue limit")
	}
	// Only one relay can bind the loopback port, so its HTTP handlers are the only
	// appenders. Hold a process-wide lock for the size check and atomic write.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	unlock, err := flock.Lock(ctx, filepath.Join(q.dir, "append.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return err
	}
	total := added
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
	}
	if total > maxQueueBytes {
		return errors.New("desktop relay queue is full")
	}
	created := make([]string, 0, len(items))
	for _, item := range items {
		var nonce [8]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			removeQueued(created)
			return err
		}
		name := fmt.Sprintf("%020d-%s.json", time.Now().UnixNano(), hex.EncodeToString(nonce[:]))
		path := filepath.Join(q.dir, name)
		if err := config.WriteFileAtomic(path, item, 0o600); err != nil {
			removeQueued(created)
			return err
		}
		created = append(created, path)
	}
	return nil
}

func removeQueued(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

// Pending returns the number of queued batches and bytes for status and diagnostics.
func (q *Queue) Pending() (int, int64, error) {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return 0, 0, err
	}
	var count int
	var bytes int64
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return 0, 0, err
		}
		count++
		bytes += info.Size()
	}
	return count, bytes, nil
}

// Drain sends queued batches in creation order. A failed batch stays on disk; other
// projects still get a chance to deliver. Only one process drains at a time.
func (q *Queue) Drain(ctx context.Context, send func(context.Context, string, []byte) error) (int, error) {
	unlock, err := flock.TryLock(filepath.Join(q.dir, "delivery.lock"))
	if flock.IsBusy(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer unlock()
	// Take the append lock only for the snapshot. A multi-project request cannot
	// appear half-written in a delivery pass, and network calls hold no append lock.
	lockCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	appendUnlock, err := flock.Lock(lockCtx, filepath.Join(q.dir, "append.lock"))
	cancel()
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(q.dir)
	appendUnlock()
	if err != nil {
		return 0, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var delivered int
	var firstErr error
	for _, entry := range entries {
		if ctx.Err() != nil {
			return delivered, ctx.Err()
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(q.dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			firstErr = errors.Join(firstErr, err)
			continue
		}
		var item queueItem
		if err := json.Unmarshal(data, &item); err != nil || !project.ValidID(item.ProjectID) || len(item.Payload) == 0 {
			firstErr = errors.Join(firstErr, fmt.Errorf("invalid desktop relay queue item %s", entry.Name()))
			continue
		}
		if time.Since(item.AddedAt) > queueMaxAge {
			if err := os.Remove(path); err != nil {
				firstErr = errors.Join(firstErr, err)
			}
			continue
		}
		if err := send(ctx, item.ProjectID, item.Payload); err != nil {
			firstErr = errors.Join(firstErr, err)
			continue
		}
		if err := os.Remove(path); err != nil {
			firstErr = errors.Join(firstErr, err)
			continue
		}
		delivered++
	}
	return delivered, firstErr
}
