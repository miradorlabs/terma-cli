package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// A stopping relay leaves what it still holds beside the outbox, for the next relay to hold
// on: a session claimed seconds after a restart would otherwise lose its first records. A
// running relay writes nothing unclaimed; only a stop does, and the next start reads it
// back and removes it:
//
//	<dir>/.held/<held at>-<seq>.held   one part: a JSON heldMeta line, then the OTLP body
//	<dir>/.held/index.json             what the held parts' traces and processes had named
//
// <held at> is zero-padded Unix nanoseconds, so a name sort is the order parts were held
// in. The dot keeps the outbox's routes and janitor out. The hold's bounds and ages carry
// over: a part's hold runs from when it was first held, across the restart.
const (
	heldDir    = ".held"
	heldSuffix = ".held"
	heldIndex  = "index.json"
)

// heldMeta is what a held part needs, beside its body, to be decided again.
type heldMeta struct {
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
// trace was named before the stop would wait for a naming that already came.
type heldNames struct {
	Traces map[string]string   `json:"traces,omitempty"`
	Procs  map[string]heldProc `json:"procs,omitempty"`
}

type heldProc struct {
	Sessions []string  `json:"sessions"`
	Overflow bool      `json:"overflow,omitempty"`
	ExitedAt time.Time `json:"exited_at,omitzero"`
}

func (r *Relay) heldPath() string { return filepath.Join(r.opts.Dir, heldDir) }

// saveHeldLocked writes every held part and empties the hold, counting what it wrote as
// held at exit and dropping what it could not. deliverMu and mu are held.
func (r *Relay) saveHeldLocked() {
	type entry struct {
		key string
		h   heldPart
	}
	var all []entry
	for key, parts := range r.held {
		for _, h := range parts {
			all = append(all, entry{key, h})
		}
	}
	// Stable, so a session's parts keep their order where they share a time.
	slices.SortStableFunc(all, func(a, b entry) int { return a.h.at.Compare(b.h.at) })
	dir := r.heldPath()
	ready := len(all) > 0 && r.opts.Dir != "" && os.MkdirAll(dir, 0o700) == nil
	names := heldNames{Traces: map[string]string{}, Procs: map[string]heldProc{}}
	for i, e := range all {
		p := e.h.p
		if ready && r.writeHeld(dir, i, e.key, e.h) == nil {
			r.stats.add("held_at_exit."+string(p.signal), p.records)
			if id, ok := strings.CutPrefix(e.key, tracePrefix); ok && r.traces[id].session != "" {
				names.Traces[id] = r.traces[id].session
			}
			pid := p.pid
			if n, ok := strings.CutPrefix(e.key, procPrefix); ok {
				pid, _ = strconv.Atoi(n)
			}
			if st := r.procs[pid]; st != nil {
				names.Procs[strconv.Itoa(pid)] = heldProc{Sessions: sortedKeys(st.sessions), Overflow: st.overflow, ExitedAt: st.exitedAt}
			}
			continue
		}
		reason := "unclaimed_at_exit"
		if strings.HasPrefix(e.key, tracePrefix) {
			reason = "no_session_trace_at_exit"
		}
		r.stats.dropped(p.signal, reason, p.records)
	}
	if ready && (len(names.Traces) > 0 || len(names.Procs) > 0) {
		if data, err := json.Marshal(names); err == nil {
			_ = config.WriteFileAtomicNoSync(filepath.Join(dir, heldIndex), data, 0o600)
		}
	}
	clear(r.held)
	r.heldN, r.heldBytes = 0, 0
}

func (r *Relay) writeHeld(dir string, seq int, key string, h heldPart) error {
	body, err := proto.Marshal(h.p.msg)
	if err != nil {
		return err
	}
	meta, err := json.Marshal(heldMeta{Session: key, Signal: h.p.signal, Records: h.p.records, PID: h.p.pid,
		At: h.p.at, HeldAt: h.at, Start: h.p.start, Narrow: h.p.narrow})
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%020d-%06d%s", h.at.UnixNano(), seq, heldSuffix)
	return config.WriteFileAtomicNoSync(filepath.Join(dir, name), slices.Concat(meta, []byte{'\n'}, body), 0o600)
}

// loadHeld holds again what the last relay left, within the hold's bounds, and removes it
// from disk: from here it is in memory, as if never stopped.
func (r *Relay) loadHeld() {
	if r.opts.Dir == "" {
		return
	}
	dir := r.heldPath()
	des, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names heldNames
	if data, err := os.ReadFile(filepath.Join(dir, heldIndex)); err == nil {
		_ = json.Unmarshal(data, &names)
	}
	now := r.opts.Now()
	for id, session := range names.Traces {
		r.learnTrace(id, session)
	}
	r.mu.Lock()
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
	r.mu.Unlock()
	// A name sort is the order the parts were held in. A part is held only once its file
	// is gone, so one Windows will not let go of is never held, and sent, twice.
	for _, de := range des {
		path := filepath.Join(dir, de.Name())
		if !de.Type().IsRegular() || !strings.HasSuffix(de.Name(), heldSuffix) {
			_ = os.Remove(path) // the index, or a write a crash cut short
			continue
		}
		h, key, err := readHeld(path)
		gone := os.Remove(path) == nil
		switch {
		case err != nil:
			r.stats.add("held_unreadable", 1)
		case !gone:
			r.stats.dropped(h.p.signal, "held_unremovable", h.p.records)
		case r.holdSince(key, h):
			r.stats.add("recovered_held."+string(h.p.signal), h.p.records)
		}
	}
	_ = os.Remove(dir)
}

func readHeld(path string) (heldPart, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return heldPart{}, "", err
	}
	line, body, ok := bytes.Cut(data, []byte{'\n'})
	var m heldMeta
	if !ok || json.Unmarshal(line, &m) != nil || m.Session == "" || m.Records <= 0 {
		return heldPart{}, "", errors.New("unreadable held part")
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
		return heldPart{}, "", errors.New("unknown signal")
	}
	if err := proto.Unmarshal(body, msg); err != nil {
		return heldPart{}, "", err
	}
	p := &part{signal: m.Signal, session: m.Session, msg: msg, records: m.Records, pid: m.PID, at: m.At, start: m.Start, narrow: m.Narrow}
	return heldPart{p: p, at: m.HeldAt, size: len(body)}, m.Session, nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
