package relay

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// The held store mirrors the hold on disk, so a relay that stops, crashes or is killed
// loses at most its last second: a session claimed seconds after a restart would
// otherwise lose its first records. It is written behind, off the export path. Holding
// and letting go only mark parts in memory; once a second the relay's ticker flushes,
// writing what was held since as one batch file and removing, or compacting, each batch
// whose parts left the hold. Nothing is synced, and nothing outlives its hold by more than
// a flush, so the hold's bounds are the store's.
//
//	<dir>/.held/<seq>.held    a batch: frames of [uvarint len][heldMeta JSON][uvarint len][OTLP body]
//	<dir>/.held/index.json    what the held parts' traces and processes had named
//
// <seq> is zero-padded Unix nanoseconds of the flush. The dot keeps the outbox's routes
// and janitor out.
const (
	heldDir    = ".held"
	heldSuffix = ".held"
	heldIndex  = "index.json"
)

// heldMeta is what a held part needs, beside its body, to be decided again.
type heldMeta struct {
	// ID tells a part's copies apart from another part's: a crash between a compaction's
	// write and its removal leaves two copies, and only one is held again.
	ID      uint64    `json:"id"`
	Session string    `json:"session"`
	Signal  Signal    `json:"signal"`
	Records int       `json:"records"`
	PID     int       `json:"pid,omitempty"`
	At      time.Time `json:"at"`
	HeldAt  time.Time `json:"held_at"`
	Start   bool      `json:"start,omitempty"`
	Narrow  bool      `json:"narrow,omitempty"`
}

// heldNames is what the held parts' traces and senders had named: without it a span whose
// trace was named before the restart would wait for a naming that already came.
type heldNames struct {
	Traces map[string]string   `json:"traces,omitempty"`
	Procs  map[string]heldProc `json:"procs,omitempty"`
}

type heldProc struct {
	Sessions []string  `json:"sessions"`
	Overflow bool      `json:"overflow,omitempty"`
	ExitedAt time.Time `json:"exited_at,omitzero"`
}

// heldStore is the store's account of which held part is in which batch. Every change
// but a flush's file work runs under deliverMu, as the hold's own changes do.
type heldStore struct {
	dir     string
	flushMu sync.Mutex // one flush at a time
	mu      sync.Mutex
	pending []heldEntry       // held since the last flush, in hold order
	queued  map[*part]bool    // pending, and still held
	on      map[*part]frameAt // written, and still held
	dirty   map[*heldBatch]bool
	stale   []string // batch files a failed removal left behind
	names   []byte   // the index as last written
	seq     int
	nextID  uint64 // from the clock at start, so no earlier relay's part has it
}

type heldEntry struct {
	id   uint64
	key  string
	h    heldPart
	body []byte
}

type heldBatch struct {
	name   string
	group  string
	frames []heldFrame
	live   int
}

// heldFrame is one part's bytes in its batch file.
type heldFrame struct {
	p        *part
	off, end int
	gone     bool
}

type frameAt struct {
	b *heldBatch
	i int
}

func newHeldStore(dir string) heldStore {
	if dir != "" {
		dir = filepath.Join(dir, heldDir)
	}
	return heldStore{dir: dir, nextID: uint64(time.Now().UnixNano()), queued: map[*part]bool{}, on: map[*part]frameAt{}, dirty: map[*heldBatch]bool{}}
}

// add marks a newly held part, with its encoding, for the next flush.
func (s *heldStore) add(key string, h heldPart, body []byte) {
	if s.dir == "" || body == nil {
		return
	}
	s.mu.Lock()
	s.nextID++
	s.pending = append(s.pending, heldEntry{s.nextID, key, h, body})
	s.queued[h.p] = true
	s.mu.Unlock()
}

// gone marks a part that left the hold, released or dropped: the next flush removes its copy.
func (s *heldStore) gone(p *part) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queued[p] {
		delete(s.queued, p)
		return
	}
	if at, ok := s.on[p]; ok {
		delete(s.on, p)
		at.b.frames[at.i].gone = true
		at.b.live--
		s.dirty[at.b] = true
	}
}

func metaOf(id uint64, key string, h heldPart) heldMeta {
	return heldMeta{ID: id, Session: key, Signal: h.p.signal, Records: h.p.records, PID: h.p.pid,
		At: h.p.at, HeldAt: h.at, Start: h.p.start, Narrow: h.p.narrow}
}

func appendFrame(buf []byte, m heldMeta, body []byte) ([]byte, error) {
	meta, err := json.Marshal(m)
	if err != nil {
		return buf, err
	}
	buf = binary.AppendUvarint(buf, uint64(len(meta)))
	buf = append(buf, meta...)
	buf = binary.AppendUvarint(buf, uint64(len(body)))
	return append(buf, body...), nil
}

// batchBuild is one new batch file: one group's parts held since the last flush, then
// what its compacted batches still hold.
type batchBuild struct {
	group  string
	buf    []byte
	frames []heldFrame
	src    []frameAt // where each frame was, b nil for a pending part
	old    []*heldBatch
}

func (bb *batchBuild) add(p *part, frame []byte, src frameAt) {
	off := len(bb.buf)
	bb.buf = append(bb.buf, frame...)
	bb.frames = append(bb.frames, heldFrame{p: p, off: off, end: len(bb.buf)})
	bb.src = append(bb.src, src)
}

// heldGroupLocked is the batch a part under key belongs in: one per session and hold
// length, so a batch's parts mostly leave together and its file goes whole, not rewritten
// for the ones that stay. r.mu is held.
func (r *Relay) heldGroupLocked(key string, start bool) string {
	if id, ok := strings.CutPrefix(key, tracePrefix); ok {
		return "t:" + r.traces[id].session // traces no record named share one
	}
	if strings.HasPrefix(key, procPrefix) {
		return "t:" + procPrefix
	}
	if start {
		return "t:" + key
	}
	return "s:" + key
}

// flushHeld brings the store in line with the hold. The parts held since the last flush
// go to a new batch file per group, with what each batch others left still holds; emptied
// and compacted batches are removed. The relay's ticker runs it, never an export, and it
// holds up neither: it takes the hold's locks only to copy and to commit.
func (r *Relay) flushHeld() {
	s := &r.store
	if s.dir == "" {
		return
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	// What the hold is now, copied under its locks: a part that is still held is changed by no one.
	type snap struct {
		p     *part
		group string
		meta  heldMeta
		body  []byte
	}
	var fresh []snap
	r.mu.Lock()
	s.mu.Lock()
	names := r.heldNamesLocked()
	for _, e := range s.pending {
		if s.queued[e.h.p] {
			fresh = append(fresh, snap{e.h.p, r.heldGroupLocked(e.key, e.h.p.start), metaOf(e.id, e.key, e.h), e.body})
		}
	}
	r.mu.Unlock()
	var empty []*heldBatch
	type compaction struct {
		b    *heldBatch
		live []int
	}
	var compact []compaction
	for b := range s.dirty {
		c := compaction{b: b}
		for i, f := range b.frames {
			if !f.gone {
				c.live = append(c.live, i)
			}
		}
		if len(c.live) == 0 {
			empty = append(empty, b)
		} else {
			compact = append(compact, c)
		}
	}
	s.mu.Unlock()

	// The files, with no lock held: a batch file never changes once written.
	builds := map[string]*batchBuild{}
	build := func(group string) *batchBuild {
		if builds[group] == nil {
			builds[group] = &batchBuild{group: group}
		}
		return builds[group]
	}
	for _, f := range fresh {
		if frame, err := appendFrame(nil, f.meta, f.body); err == nil { // else it stays in memory only
			build(f.group).add(f.p, frame, frameAt{})
		}
	}
	for _, c := range compact {
		data, err := os.ReadFile(filepath.Join(s.dir, c.b.name))
		if err != nil {
			continue // stays dirty, tried again next flush
		}
		bb := build(c.b.group)
		for _, i := range c.live {
			f := c.b.frames[i]
			bb.add(f.p, data[f.off:f.end], frameAt{c.b, i})
		}
		bb.old = append(bb.old, c.b)
	}
	var written []*batchBuild
	var names2 []string
	for _, bb := range builds {
		name, err := r.writeBatch(bb.buf)
		if err != nil {
			r.stats.add("held_store_write_failed", 1)
			r.warnf("held store %s: %v; held records stay in memory only", s.dir, err)
			continue
		}
		written = append(written, bb)
		names2 = append(names2, name)
	}

	// Commit: a part that left while its batch was written leaves a gone frame behind.
	var remove []string
	s.mu.Lock()
	for k, bb := range written {
		nb := &heldBatch{name: names2[k], group: bb.group, frames: bb.frames}
		for i := range nb.frames {
			f, src := &nb.frames[i], bb.src[i]
			if src.b == nil {
				f.gone = !s.queued[f.p]
				delete(s.queued, f.p)
			} else {
				f.gone = s.on[f.p] != src
			}
			if !f.gone {
				s.on[f.p] = frameAt{nb, i}
				nb.live++
			}
		}
		if nb.live < len(nb.frames) {
			s.dirty[nb] = true
		}
		for _, b := range bb.old {
			delete(s.dirty, b)
			remove = append(remove, b.name)
		}
	}
	s.pending = slices.DeleteFunc(s.pending, func(e heldEntry) bool { return !s.queued[e.h.p] })
	for _, b := range empty {
		delete(s.dirty, b)
		remove = append(remove, b.name)
	}
	remove = append(remove, s.stale...)
	s.stale = nil
	writeNames := !slices.Equal(names, s.names)
	s.mu.Unlock()

	var stale []string
	for _, name := range remove {
		if err := os.Remove(filepath.Join(s.dir, name)); err == nil {
			r.stats.add("held_store_files_removed", 1)
		} else if !errors.Is(err, fs.ErrNotExist) {
			stale = append(stale, name)
		}
	}
	if writeNames {
		r.writeHeldNames(names)
	}
	if len(stale) > 0 {
		s.mu.Lock()
		s.stale = append(s.stale, stale...)
		s.mu.Unlock()
	}
}

func (r *Relay) writeBatch(buf []byte) (string, error) {
	s := &r.store
	// Never the outbox above it, which New creates: a relay torn down mid-flush leaves nothing behind.
	if err := os.Mkdir(s.dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	s.seq++
	name := fmt.Sprintf("%020d-%06d%s", time.Now().UnixNano(), s.seq%1_000_000, heldSuffix)
	if err := config.WriteFileAtomicNoSync(filepath.Join(s.dir, name), buf, 0o600); err != nil {
		return "", err
	}
	r.stats.add("held_store_files_written", 1)
	r.stats.add("held_store_bytes_written", len(buf))
	return name, nil
}

func (r *Relay) writeHeldNames(names []byte) {
	s := &r.store
	path := filepath.Join(s.dir, heldIndex)
	var err error
	if names != nil {
		err = config.WriteFileAtomicNoSync(path, names, 0o600)
	} else if err = os.Remove(path); errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err == nil {
		s.mu.Lock()
		s.names = names
		s.mu.Unlock()
	}
}

// heldNamesLocked encodes what the held parts' traces and senders named, nil when none
// did. r.mu is held.
func (r *Relay) heldNamesLocked() []byte {
	names := heldNames{Traces: map[string]string{}, Procs: map[string]heldProc{}}
	for key, parts := range r.held {
		if id, ok := strings.CutPrefix(key, tracePrefix); ok && r.traces[id].session != "" {
			names.Traces[id] = r.traces[id].session
		}
		for _, h := range parts {
			pid := h.p.pid
			if n, ok := strings.CutPrefix(key, procPrefix); ok {
				pid, _ = strconv.Atoi(n)
			}
			if st := r.procs[pid]; st != nil {
				names.Procs[strconv.Itoa(pid)] = heldProc{Sessions: sortedKeys(st.sessions), Overflow: st.overflow, ExitedAt: st.exitedAt}
			}
		}
	}
	if len(names.Traces) == 0 && len(names.Procs) == 0 {
		return nil
	}
	data, _ := json.Marshal(names) // maps of strings: encodes, and in key order
	return data
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
