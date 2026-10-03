package relay

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// loadHeld holds again what the last relay's store kept, within the hold's bounds and in
// the order it was first held. The batches stay where they are, as this relay's own.
func (r *Relay) loadHeld() {
	s := &r.store
	if s.dir == "" {
		return
	}
	des, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	if data, err := os.ReadFile(filepath.Join(s.dir, heldIndex)); err == nil {
		r.restoreNames(data)
		s.names = data
	}
	type loaded struct {
		key string
		h   heldPart
		at  frameAt
	}
	var all []loaded
	seen := map[uint64]bool{}
	for _, de := range des {
		name := de.Name()
		if !de.Type().IsRegular() || !strings.HasSuffix(name, heldSuffix) {
			if name != heldIndex {
				_ = os.Remove(filepath.Join(s.dir, name)) // a write a crash cut short
			}
			continue
		}
		b := &heldBatch{name: name}
		entries, err := readBatch(filepath.Join(s.dir, name))
		if err != nil {
			r.stats.add("held_unreadable", 1)
			s.dirty[b] = true // what it held before the damage is kept, the rest goes
		}
		for _, e := range entries {
			if seen[e.id] {
				b.frames = append(b.frames, heldFrame{p: e.frame.p, gone: true})
				s.dirty[b] = true
				continue
			}
			seen[e.id] = true
			if len(b.frames) == 0 {
				b.group = r.heldGroupLocked(e.key, e.h.p.start) // nothing else runs yet
			}
			b.frames = append(b.frames, e.frame)
			all = append(all, loaded{e.key, e.h, frameAt{b, len(b.frames) - 1}})
		}
		if len(b.frames) == 0 {
			s.dirty[b] = true
		}
	}
	// A compaction writes older parts after newer ones; held order is held_at's.
	slices.SortStableFunc(all, func(a, b loaded) int { return a.h.at.Compare(b.h.at) })
	for _, l := range all {
		if r.holdSince(l.key, l.h) {
			s.on[l.h.p] = l.at
			l.at.b.live++
			r.stats.add("recovered_held."+string(l.h.p.signal), l.h.p.records)
		} else {
			l.at.b.frames[l.at.i].gone = true
			s.dirty[l.at.b] = true
		}
	}
}

func (r *Relay) restoreNames(data []byte) {
	var names heldNames
	if json.Unmarshal(data, &names) != nil {
		return
	}
	for id, session := range names.Traces {
		r.learnTrace(id, session)
	}
	now := r.opts.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for n, hp := range names.Procs {
		pid, err := strconv.Atoi(n)
		if err != nil || pid == 0 || len(r.procs) >= maxProcs {
			continue
		}
		st := &procState{sessions: map[string]bool{}, overflow: hp.Overflow, lastSeen: now, exitedAt: hp.ExitedAt}
		for _, s := range hp.Sessions {
			st.sessions[s] = true
		}
		r.procs[pid] = st
	}
}

type batchEntry struct {
	id    uint64
	key   string
	h     heldPart
	frame heldFrame
}

var errBatch = errors.New("damaged held batch")

// readBatch decodes a batch file's frames, as many as are whole when it is damaged.
func readBatch(path string) ([]batchEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []batchEntry
	for off := 0; off < len(data); {
		e, end, err := decodeFrame(data, off)
		if err != nil {
			return out, err
		}
		e.frame.off, e.frame.end = off, end
		out = append(out, e)
		off = end
	}
	return out, nil
}

func decodeFrame(data []byte, off int) (batchEntry, int, error) {
	field := func() ([]byte, bool) {
		n, w := binary.Uvarint(data[off:])
		if w <= 0 || n > uint64(len(data)-off-w) {
			return nil, false
		}
		off += w
		f := data[off : off+int(n)]
		off += int(n)
		return f, true
	}
	metaBytes, ok := field()
	if !ok {
		return batchEntry{}, 0, errBatch
	}
	body, ok := field()
	var m heldMeta
	if !ok || json.Unmarshal(metaBytes, &m) != nil || m.Session == "" || m.Records <= 0 {
		return batchEntry{}, 0, errBatch
	}
	var msg proto.Message
	switch m.Signal {
	case Logs:
		msg = &logspb.LogsData{}
	case Traces:
		msg = &tracepb.TracesData{}
	case Metrics:
		msg = &metricspb.MetricsData{}
	default:
		return batchEntry{}, 0, errBatch
	}
	if proto.Unmarshal(body, msg) != nil {
		return batchEntry{}, 0, errBatch
	}
	p := &part{signal: m.Signal, session: m.Session, msg: msg, records: m.Records, pid: m.PID, at: m.At, start: m.Start, narrow: m.Narrow}
	return batchEntry{id: m.ID, key: m.Session, h: heldPart{p: p, at: m.HeldAt, size: len(body)}, frame: heldFrame{p: p}}, off, nil
}
