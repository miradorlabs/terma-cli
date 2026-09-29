package relay

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// BindingFunc names the project a directory's repository is bound to, "" when it has no
// binding. An error means the binding could not be read (a half-written file): the
// relay asks again rather than deciding the session is unbound.
type BindingFunc func(dir string) (projectID string, err error)

// Placement is where a session runs from a moment on. A session has several when it is
// resumed elsewhere: `claude --resume` or `codex exec resume` in another directory keeps
// the session id, so the relay places each record by the placement in effect at the
// record's own time — the resumed run goes where it runs, the earlier run's records stay.
type Placement struct {
	Since time.Time // zero: since the session began
	Dir   string
}

// DirsFunc reads a session's placements from the agent's own files, starting at offset
// from (a byte offset it returned before, 0 the first time), and returns the ones found
// and the offset to continue from. source is the convention that named the session.
type DirsFunc func(ctx context.Context, session, source string, from int64) ([]Placement, int64)

// resolver decides which route each of a session's placements takes. A decision is
// final once made and is kept on disk (routes/<session>, one line per placement), so a
// session's records never move between projects — not even across a relay restart.
type resolver struct {
	dir     string // the relay's state directory
	binding BindingFunc
	dirs    DirsFunc
	logf    func(string, ...any)

	mu      sync.Mutex
	decided map[string][]decision // session → a route per placement, by since
	agent   map[string]*agentDirs // session → what its agent's files said so far
}

type decision struct {
	since time.Time
	route string
}

type agentDirs struct {
	found  []Placement
	offset int64
	last   time.Time // when they were last read
}

func newResolver(dir string, binding BindingFunc, dirs DirsFunc, logf func(string, ...any)) *resolver {
	return &resolver{dir: dir, binding: binding, dirs: dirs, logf: logf,
		decided: map[string][]decision{}, agent: map[string]*agentDirs{}}
}

// retryLookup is how often a session's agent files are read again for new placements.
const retryLookup = time.Second

// placements are session's placements, oldest first: those session-start hooks recorded
// (sessions/<id>) and those its agent's own files show (a Codex rollout's turn
// contexts), with a repeat of the same directory collapsed into the earlier one.
//
// The agent's files are read again at most every retryLookup — and at once for a record
// stamped after the last read, which may belong to a turn started since: a resumed-
// elsewhere Codex turn records its directory at the turn's start, before any of its
// records, so reading again then never places it by a stale history.
func (r *resolver) placements(ctx context.Context, session, source string, t, now time.Time) []Placement {
	ps := readPlacements(r.dir, session)
	if r.dirs != nil {
		r.mu.Lock()
		a := r.agent[session]
		if a == nil {
			a = &agentDirs{}
			r.agent[session] = a
		}
		due := now.Sub(a.last) >= retryLookup || (!t.IsZero() && t.After(a.last))
		if due {
			a.last = now
		}
		offset := a.offset
		r.mu.Unlock()
		if due {
			lookup, cancel := context.WithTimeout(ctx, 2*time.Second)
			found, next := r.dirs(lookup, session, source, offset)
			cancel()
			r.mu.Lock()
			a.found, a.offset = append(a.found, found...), next
			r.mu.Unlock()
		}
		r.mu.Lock()
		ps = append(ps, a.found...)
		r.mu.Unlock()
	}
	slices.SortStableFunc(ps, func(a, b Placement) int { return a.Since.Compare(b.Since) })
	out := ps[:0]
	for _, p := range ps {
		if len(out) > 0 && out[len(out)-1].Dir == p.Dir {
			continue
		}
		out = append(out, p)
	}
	return out
}

// at is the placement in effect at t: the latest that began at or before it, else the
// first (a record stamped a moment before its session-start hook ran).
func at(ps []Placement, t time.Time) Placement {
	chosen := ps[0]
	for _, p := range ps[1:] {
		if !t.IsZero() && p.Since.After(t) {
			break
		}
		chosen = p
	}
	return chosen
}

// decide returns the route for a record of session stamped t, and whether it is final.
// A session with no placement is not decided until final is forced (its records' hold
// closed), and then goes to the machine project.
func (r *resolver) decide(ctx context.Context, session, source string, t, now time.Time, force bool) (string, bool) {
	ps := r.placements(ctx, session, source, t, now)
	decided := r.decisions(session)
	if len(ps) == 0 {
		if len(decided) > 0 {
			return decided[len(decided)-1].route, true
		}
		if !force {
			return "", false
		}
		return r.settle(session, time.Time{}, machineRoute), true
	}
	p := at(ps, t)
	for _, d := range decided {
		if d.since.Equal(p.Since) {
			return d.route, true
		}
	}
	project, err := r.binding(p.Dir)
	switch {
	case err == nil && project != "":
		return r.settle(session, p.Since, project), true
	case err == nil:
		return r.settle(session, p.Since, machineRoute), true
	case !force:
		r.logf("session %s: binding for %s unreadable, retrying: %v", session, p.Dir, err)
		return "", false
	default:
		return r.settle(session, p.Since, machineRoute), true
	}
}

// known reports whether session can be placed without waiting for its agent's files:
// it is decided, or a session-start hook has recorded where it runs.
func (r *resolver) known(session string) bool {
	if len(r.decisions(session)) > 0 {
		return true
	}
	return len(readPlacements(r.dir, session)) > 0
}

// decisions are session's decisions, from memory, else from routes/<session>.
func (r *resolver) decisions(session string) []decision {
	r.mu.Lock()
	d, ok := r.decided[session]
	r.mu.Unlock()
	if ok {
		return d
	}
	d = readDecisions(r.dir, session)
	r.mu.Lock()
	r.decided[session] = d
	r.mu.Unlock()
	return d
}

// settle records a final decision for session's placement since in memory and on disk.
func (r *resolver) settle(session string, since time.Time, route string) string {
	r.mu.Lock()
	d := append(slices.Clone(r.decided[session]), decision{since: since, route: route})
	slices.SortFunc(d, func(a, b decision) int { return a.since.Compare(b.since) })
	r.decided[session] = d
	r.mu.Unlock()
	var b strings.Builder
	for _, x := range d {
		b.WriteString(formatSince(x.since) + "\t" + x.route + "\n")
	}
	dir := filepath.Join(r.dir, routesDir)
	if err := os.MkdirAll(dir, 0o700); err == nil {
		if err := config.WriteFileAtomicNoSync(filepath.Join(dir, session), []byte(b.String()), 0o600); err != nil {
			r.logf("session %s: record route: %v", session, err)
		}
	}
	return route
}

// Session and route files are one line per placement, oldest first: "<unix nanos>\t<dir
// or route>". A line with no tab — every file an earlier build wrote — holds since the
// session began.

func formatSince(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.UnixNano(), 10)
}

func parseLine(line string) (time.Time, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return time.Time{}, "", false
	}
	nanos, value, ok := strings.Cut(line, "\t")
	if !ok {
		return time.Time{}, line, true
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return time.Time{}, "", false
	}
	if n == 0 {
		return time.Time{}, value, true
	}
	return time.Unix(0, n), value, true
}

type line struct {
	since time.Time
	value string
}

func readLines(path string) []line {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []line
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if t, v, ok := parseLine(sc.Text()); ok {
			out = append(out, line{t, v})
		}
	}
	return out
}

// readPlacements are the placements session-start hooks recorded for session.
func readPlacements(dir, session string) []Placement {
	if !validSessionID(session) {
		return nil
	}
	var out []Placement
	for _, l := range readLines(filepath.Join(dir, sessionsDir, session)) {
		if filepath.IsAbs(l.value) {
			out = append(out, Placement{Since: l.since, Dir: l.value})
		}
	}
	return out
}

func readDecisions(dir, session string) []decision {
	if !validSessionID(session) {
		return nil
	}
	var out []decision
	for _, l := range readLines(filepath.Join(dir, routesDir, session)) {
		if validRoute(l.value) {
			out = append(out, decision{since: l.since, route: l.value})
		}
	}
	return out
}

// sessionDir is the directory session was last placed in by a hook, "" when none.
func sessionDir(dir, session string) string {
	ps := readPlacements(dir, session)
	if len(ps) == 0 {
		return ""
	}
	return ps[len(ps)-1].Dir
}

// traceMap remembers which session a trace belongs to, so a span that does not name its
// session (most of Codex's) follows the one that did. It is bounded: the oldest half is
// dropped when it fills.
type traceMap struct {
	mu    sync.Mutex
	limit int
	m     map[string]traceEntry
}

type traceEntry struct {
	session, source string
	seen            time.Time
}

func newTraceMap(limit int) *traceMap {
	return &traceMap{limit: limit, m: map[string]traceEntry{}}
}

func (t *traceMap) put(trace, session, source string, now time.Time) {
	if trace == "" || session == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.m) >= t.limit {
		t.evictOldest(now)
	}
	t.m[trace] = traceEntry{session: session, source: source, seen: now}
}

func (t *traceMap) get(trace string) (string, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[trace]
	if !ok {
		return "", ""
	}
	return e.session, e.source
}

// evictOldest drops entries older than the mean age. Called with the lock held.
func (t *traceMap) evictOldest(now time.Time) {
	var total time.Duration
	for _, e := range t.m {
		total += now.Sub(e.seen)
	}
	mean := total / time.Duration(len(t.m))
	for k, e := range t.m {
		if now.Sub(e.seen) >= mean {
			delete(t.m, k)
		}
	}
	if len(t.m) >= t.limit { // every entry the same age
		clear(t.m)
	}
}
