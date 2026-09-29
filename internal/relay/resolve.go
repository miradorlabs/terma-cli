package relay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// BindingFunc names the project a directory's repository is bound to, "" when it has no
// binding. An error means the binding could not be read (a half-written file): the
// relay asks again rather than deciding the session is unbound.
type BindingFunc func(dir string) (projectID string, err error)

// CwdFunc finds the directory an agent session started in from the agent's own files,
// "" when it cannot. source is the agent convention that named the session ("codex",
// "claude").
type CwdFunc func(ctx context.Context, session, source string) string

// resolver decides which route a session's records take. A decision is final once made
// and is kept on disk (routes/<session>), so a session never moves between projects
// part-way through — not even across a relay restart.
type resolver struct {
	dir     string // the relay's state directory
	binding BindingFunc
	cwd     CwdFunc
	logf    func(string, ...any)

	mu      sync.Mutex
	decided map[string]string    // session → route
	tried   map[string]time.Time // session → last time its agent's files were searched
}

func newResolver(dir string, binding BindingFunc, cwd CwdFunc, logf func(string, ...any)) *resolver {
	return &resolver{dir: dir, binding: binding, cwd: cwd, logf: logf,
		decided: map[string]string{}, tried: map[string]time.Time{}}
}

// retryLookup is how often an undecided session's agent files are searched again.
const retryLookup = 2 * time.Second

// decide returns the session's route and whether it is final. A session with no known
// directory is not decided until final is forced (its records' hold window closed), and
// then it goes to the machine project.
func (r *resolver) decide(ctx context.Context, session, source string, now time.Time, force bool) (string, bool) {
	r.mu.Lock()
	route, ok := r.decided[session]
	r.mu.Unlock()
	if ok {
		return route, true
	}
	if route, ok := r.load(session); ok {
		r.remember(session, route)
		return route, true
	}

	dir := sessionDir(r.dir, session)
	if dir == "" && r.cwd != nil && r.shouldSearch(session, now) {
		lookup, cancel := context.WithTimeout(ctx, 2*time.Second)
		dir = r.cwd(lookup, session, source)
		cancel()
	}
	if dir != "" {
		project, err := r.binding(dir)
		switch {
		case err == nil && project != "":
			return r.settle(session, project), true
		case err == nil:
			return r.settle(session, machineRoute), true
		case !force:
			r.logf("session %s: binding for %s unreadable, retrying: %v", session, dir, err)
			return "", false
		}
	}
	if !force {
		return "", false
	}
	return r.settle(session, machineRoute), true
}

// known reports whether session can be placed now without searching an agent's files:
// it is decided, or a session-start hook has recorded its directory.
func (r *resolver) known(session string) bool {
	r.mu.Lock()
	_, ok := r.decided[session]
	r.mu.Unlock()
	if ok {
		return true
	}
	if _, err := os.Stat(filepath.Join(r.dir, routesDir, session)); err == nil {
		return true
	}
	return sessionDir(r.dir, session) != ""
}

func (r *resolver) shouldSearch(session string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.tried[session]; ok && now.Sub(last) < retryLookup {
		return false
	}
	r.tried[session] = now
	return true
}

func (r *resolver) remember(session, route string) {
	r.mu.Lock()
	r.decided[session] = route
	delete(r.tried, session)
	r.mu.Unlock()
}

// settle records a final decision in memory and on disk.
func (r *resolver) settle(session, route string) string {
	r.remember(session, route)
	dir := filepath.Join(r.dir, routesDir)
	if err := os.MkdirAll(dir, 0o700); err == nil {
		if err := config.WriteFileAtomicNoSync(filepath.Join(dir, session), []byte(route+"\n"), 0o600); err != nil {
			r.logf("session %s: record route: %v", session, err)
		}
	}
	return route
}

func (r *resolver) load(session string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(r.dir, routesDir, session))
	if err != nil {
		return "", false
	}
	route := strings.TrimSpace(string(data))
	return route, validRoute(route)
}

// sessionDir reads the directory a session-start hook recorded for session
// (RecordSession), "" when none is.
func sessionDir(dir, session string) string {
	if !validSessionID(session) {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, sessionsDir, session))
	if err != nil {
		return ""
	}
	cwd := strings.TrimSpace(string(data))
	if !filepath.IsAbs(cwd) {
		return ""
	}
	return cwd
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

// evictOldest drops entries older than the median age. Called with the lock held.
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
