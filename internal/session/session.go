// Package session records which agent sessions touched which files in a repository,
// so a commit is attributed by per-session manifests, with the active session only as
// a fallback for a session that has none.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// Session is what a harness announced when it started.
type Session struct {
	ID          string    `json:"id"`
	Tool        string    `json:"tool"`
	ToolVersion string    `json:"tool_version,omitempty"`
	Model       string    `json:"model,omitempty"`
	Cwd         string    `json:"cwd,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Manifest is the set of files one session edited, with when each was last touched.
type Manifest struct {
	SessionID   string               `json:"session_id"`
	Tool        string               `json:"tool"`
	ToolVersion string               `json:"tool_version,omitempty"`
	StartedAt   time.Time            `json:"started_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
	Files       map[string]time.Time `json:"files"`
}

// Key is a session's identity. Each harness chooses its own ids, so an id is unique only
// with the tool that chose it.
type Key struct {
	Tool string
	ID   string
}

// valid reports whether both halves are safe to persist as path components.
func (k Key) valid() bool { return ValidID(k.Tool) && ValidID(k.ID) }

// Key is the manifest's session identity.
func (m Manifest) Key() Key { return Key{Tool: m.Tool, ID: m.SessionID} }

// Key is the session's identity.
func (s Session) Key() Key { return Key{Tool: s.Tool, ID: s.ID} }

// ToolLabel renders the Agent-Tool trailer value.
func (m Manifest) ToolLabel() string { return toolLabel(m.Tool, m.ToolVersion) }

// ToolLabel renders the Agent-Tool trailer value.
func (s Session) ToolLabel() string { return toolLabel(s.Tool, s.ToolVersion) }

func toolLabel(tool, version string) string {
	if tool == "" {
		return ""
	}
	if version == "" {
		return tool
	}
	return tool + "/" + version
}

// Store is the on-disk state for one repository.
type Store struct {
	dir string
	// lockWait bounds a writer's wait for the store lock; exclusion tests raise it.
	lockWait time.Duration
}

const (
	activeFile   = "session.json"
	parkedExt    = ".jsonl"
	manifestsDir = "manifests"
	manifestExt  = ".json"
	// A delta is one touch recorded while another writer held the store: <id>~<random>.delta.
	deltaExt = ".delta"
	deltaSep = "~"
	lockFile = "store.lock"
	dirMode  = 0o700
	fileMode = 0o600
	maxIDLen = 128
)

// Open addresses the session store in dir without creating it.
func Open(dir string) *Store {
	return &Store{dir: dir, lockWait: hookLockWait}
}

// safeID is what a session id is allowed to look like when it becomes a file name.
var safeID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidID reports whether id is safe to persist and to write into a commit message.
func ValidID(id string) bool {
	return id != "" && len(id) <= maxIDLen && safeID.MatchString(id) && !strings.HasPrefix(id, ".")
}

// isMultiline reports whether a non-identifier value would break out of its trailer line.
func isMultiline(v string) bool {
	return strings.ContainsAny(v, "\r\n")
}

// SetActive records the session a harness just announced.
func (s *Store) SetActive(sess Session) error {
	if !sess.Key().valid() {
		return fmt.Errorf("invalid session %q of tool %q", sess.ID, sess.Tool)
	}
	if sess.StartedAt.IsZero() {
		sess.StartedAt = time.Now()
	}
	if sess.UpdatedAt.IsZero() {
		sess.UpdatedAt = sess.StartedAt
	}
	// Locked because Touch reads and rewrites the active session.
	unlock, _, err := s.create()
	if err != nil {
		return err
	}
	defer unlock()
	return writeJSON(filepath.Join(s.dir, activeFile), sess)
}

// Active returns the announced session when it was updated within ttl of now.
func (s *Store) Active(now time.Time, ttl time.Duration) (*Session, bool) {
	var sess Session
	if err := readJSON(filepath.Join(s.dir, activeFile), &sess); err != nil {
		return nil, false
	}
	if !ValidID(sess.ID) {
		return nil, false
	}
	if ttl > 0 && now.Sub(sess.UpdatedAt) > ttl {
		return &sess, false
	}
	return &sess, true
}

// ClearActive forgets the active session if it is k.
func (s *Store) ClearActive(k Key) error {
	if s.missing() {
		return nil
	}
	defer s.lock()()
	return s.clearActive(k)
}

// clearActive is ClearActive for a caller that already holds the store lock.
func (s *Store) clearActive(k Key) error {
	path := filepath.Join(s.dir, activeFile)
	var sess Session
	if err := readJSON(path, &sess); err != nil || sess.Key() != k {
		return nil
	}
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// hookLockWait bounds a hook's wait: holders rewrite a few kilobytes.
const hookLockWait = 250 * time.Millisecond

// lock serializes every writer of the store, since concurrent hooks read, edit and
// rename the same manifest. Never nest it: a second flock waits on the first even in one
// process. A lock that cannot be had returns a no-op, so the write still happens.
func (s *Store) lock() (unlock func()) {
	unlock, _ = s.acquire()
	return unlock
}

// missing reports a store nothing has created yet. Its lock file cannot be opened, so a
// writer that went on would do so unlocked, racing the Touch that creates the store; with
// no store there is nothing to change.
func (s *Store) missing() bool {
	_, err := os.Stat(s.dir)
	return errors.Is(err, fs.ErrNotExist)
}

// create makes the store and takes its lock, as lock does. A Retire can remove the store
// between the two, so a lock lost that way is tried again on a store made anew.
func (s *Store) create() (unlock func(), held bool, err error) {
	for range 3 {
		if err := os.MkdirAll(s.dir, dirMode); err != nil {
			return nil, false, err
		}
		if unlock, held = s.acquire(); held || !s.missing() {
			return unlock, held, nil
		}
		unlock()
	}
	return func() {}, false, nil
}

// acquire is lock, reporting whether the lock is held.
func (s *Store) acquire() (unlock func(), held bool) {
	ctx, cancel := context.WithTimeout(context.Background(), s.lockWait)
	defer cancel()
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, lockFile))
	if err != nil {
		return func() {}, false
	}
	return unlock, true
}

// Touch records that sess edited files (repo-relative) at at, and refreshes the
// active session's UpdatedAt so the TTL fallback tracks activity.
func (s *Store) Touch(sess Session, files []string, at time.Time) error {
	if !sess.Key().valid() {
		return fmt.Errorf("invalid session %q of tool %q", sess.ID, sess.Tool)
	}
	unlock, held, err := s.create()
	if err != nil {
		return err
	}
	defer unlock()
	touched := newManifest(sess, at)
	for _, f := range files {
		if f = Normalize(f); f != "" {
			touched.Files[f] = at
		}
	}
	if !held {
		// Another writer's read-modify-write would overwrite this one's: a delta of its
		// own survives, and the next locked writer folds it in.
		return s.writeDelta(touched)
	}
	m, deltas, err := s.load(sess.Key())
	if err != nil {
		return err
	}
	m = fold(m, touched)
	if err := writeJSON(s.manifestPath(sess.Key()), m); err != nil {
		return err
	}
	removeAll(deltas)
	if active, _ := s.Active(at, 0); active != nil && active.Key() == sess.Key() {
		active.UpdatedAt = at
		_ = writeJSON(filepath.Join(s.dir, activeFile), active)
	}
	return nil
}

func newManifest(sess Session, at time.Time) *Manifest {
	return &Manifest{SessionID: sess.ID, Tool: sess.Tool, ToolVersion: sess.ToolVersion, StartedAt: at, UpdatedAt: at, Files: map[string]time.Time{}}
}

// fold adds src's files to dst (the later touch wins) and returns dst, or a copy of src
// when dst is nil.
func fold(dst, src *Manifest) *Manifest {
	if dst == nil {
		dst = &Manifest{SessionID: src.SessionID, Tool: src.Tool, ToolVersion: src.ToolVersion, StartedAt: src.StartedAt, Files: map[string]time.Time{}}
	}
	if dst.StartedAt.IsZero() || !src.StartedAt.IsZero() && src.StartedAt.Before(dst.StartedAt) {
		dst.StartedAt = src.StartedAt
	}
	if src.UpdatedAt.After(dst.UpdatedAt) {
		dst.UpdatedAt = src.UpdatedAt
	}
	for f, touched := range src.Files {
		if prev, ok := dst.Files[f]; !ok || touched.After(prev) {
			dst.Files[f] = touched
		}
	}
	return dst
}

// writeDelta records m under a name no other writer can choose.
func (s *Store) writeDelta(m *Manifest) error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	return writeJSON(filepath.Join(s.dir, manifestsDir, m.Tool, m.SessionID+deltaSep+hex.EncodeToString(b[:])+deltaExt), m)
}

// load is k's manifest with its deltas folded in, and the deltas' paths, which a locked
// writer removes once it has written the result. A delta that appears later is kept.
func (s *Store) load(k Key) (*Manifest, []string, error) {
	m, err := s.manifest(k)
	if err != nil {
		return nil, nil, err
	}
	paths, _ := filepath.Glob(filepath.Join(s.dir, manifestsDir, k.Tool, k.ID+deltaSep+"*"+deltaExt))
	var folded []string
	for _, p := range paths {
		var d Manifest
		if readJSON(p, &d) != nil || d.Key() != k {
			continue
		}
		if d.Files == nil {
			d.Files = map[string]time.Time{}
		}
		m = fold(m, &d)
		folded = append(folded, p)
	}
	return m, folded, nil
}

func removeAll(paths []string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
}

// Manifests lists every recorded session, deltas folded in, oldest first.
func (s *Store) Manifests() ([]Manifest, error) {
	out, _, err := s.manifests()
	return out, err
}

// manifests is Manifests, with each session's delta paths. A tool's manifests are in a
// folder of its name.
func (s *Store) manifests() ([]Manifest, map[Key][]string, error) {
	tools, err := os.ReadDir(filepath.Join(s.dir, manifestsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	byKey := map[Key]*Manifest{}
	deltas := map[Key][]string{}
	for _, t := range tools {
		if t.IsDir() && ValidID(t.Name()) {
			s.readTool(t.Name(), byKey, deltas)
		}
	}
	out := make([]Manifest, 0, len(byKey))
	for _, m := range byKey {
		// ToolVersion reaches the commit message too: no line breaks.
		if isMultiline(m.ToolVersion) {
			m.ToolVersion = ""
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, deltas, nil
}

// readTool folds the manifests and deltas in tool's folder into byKey and deltas.
func (s *Store) readTool(tool string, byKey map[Key]*Manifest, deltas map[Key][]string) {
	dir := filepath.Join(s.dir, manifestsDir, tool)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		var id string
		switch {
		case strings.HasSuffix(name, deltaExt):
			var named bool
			if id, _, named = strings.Cut(strings.TrimSuffix(name, deltaExt), deltaSep); !named {
				continue // not a name writeDelta makes, so nothing would fold it
			}
		case strings.HasSuffix(name, manifestExt):
			id = strings.TrimSuffix(name, manifestExt)
		default:
			continue
		}
		var m Manifest
		if err := readJSON(filepath.Join(dir, name), &m); err != nil {
			continue // a torn write is skipped, never fatal to a commit
		}
		// Revalidate on read: the key reaches a commit message (a newline forges trailers)
		// and Prune's manifestPath ("../../x" deletes elsewhere), so it must match its path.
		k := m.Key()
		if !k.valid() || k != (Key{Tool: tool, ID: id}) {
			continue
		}
		if m.Files == nil {
			m.Files = map[string]time.Time{}
		}
		if strings.HasSuffix(name, deltaExt) {
			deltas[k] = append(deltas[k], filepath.Join(dir, name))
		}
		byKey[k] = fold(byKey[k], &m)
	}
}

// Consume removes files a commit carried from a session's manifest; an emptied manifest
// is kept as evidence the session reports edits, so the fallback never claims for it.
func (s *Store) Consume(k Key, files []string) error {
	if s.missing() {
		return nil
	}
	defer s.lock()()
	m, deltas, err := s.load(k)
	if err != nil || m == nil {
		return err
	}
	for _, f := range files {
		delete(m.Files, Normalize(f))
	}
	if err := writeJSON(s.manifestPath(k), m); err != nil {
		return err
	}
	removeAll(deltas)
	return nil
}

// Merge folds the manifest of into's tool's session fromID into into's (later touch wins),
// retires it and its active record, and returns the files that moved, sorted.
func (s *Store) Merge(fromID string, into Session, at time.Time) ([]string, error) {
	if !into.Key().valid() {
		return nil, fmt.Errorf("invalid session %q of tool %q", into.ID, into.Tool)
	}
	from := Key{Tool: into.Tool, ID: fromID}
	if !ValidID(fromID) || fromID == into.ID || s.missing() {
		return nil, nil
	}
	defer s.lock()()
	src, srcDeltas, err := s.load(from)
	if err != nil || src == nil {
		return nil, err
	}
	dst, dstDeltas, err := s.load(into.Key())
	if err != nil {
		return nil, err
	}
	if dst == nil {
		dst = newManifest(into, at)
	}
	started, updated := dst.StartedAt, dst.UpdatedAt
	dst = fold(dst, src)
	dst.StartedAt, dst.UpdatedAt = started, updated
	if at.After(dst.UpdatedAt) {
		dst.UpdatedAt = at
	}
	files := make([]string, 0, len(src.Files))
	for f := range src.Files {
		files = append(files, f)
	}
	// Target first: a hook killed in between leaves the files in both manifests, not neither.
	if err := writeJSON(s.manifestPath(into.Key()), dst); err != nil {
		return nil, err
	}
	removeAll(dstDeltas)
	if err := os.Remove(s.manifestPath(from)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	removeAll(srcDeltas)
	_ = s.clearActive(from)
	slices.Sort(files)
	return files, nil
}

// Prune drops manifests not updated since before, and a stale active session.
func (s *Store) Prune(before time.Time) (int, error) {
	if s.missing() {
		return 0, nil
	}
	// Locked, so a manifest a Touch just revived is not removed as stale.
	defer s.lock()()
	return s.prune(before)
}

// Retire is Prune that also removes the store once nothing is left in it and it was
// created before before; it gives up when another writer holds the store.
func (s *Store) Retire(before time.Time) {
	if s.missing() {
		return
	}
	unlock, held := s.acquire()
	if !held {
		unlock()
		return
	}
	_, err := s.prune(before)
	tools, _ := os.ReadDir(filepath.Join(s.dir, manifestsDir))
	for _, t := range tools {
		dir := filepath.Join(s.dir, manifestsDir, t.Name())
		removeTemps(dir, before)
		_ = os.Remove(dir) // only when empty
	}
	removeTemps(s.dir, before)
	_ = os.Remove(filepath.Join(s.dir, manifestsDir)) // only when empty
	entries, _ := os.ReadDir(s.dir)
	lock, statErr := os.Stat(filepath.Join(s.dir, lockFile))
	if err != nil || statErr != nil || len(entries) != 1 || !lock.ModTime().Before(before) {
		unlock()
		return
	}
	flock.Remove(filepath.Join(s.dir, lockFile), unlock)
	_ = os.Remove(s.dir) // fails, harmlessly, once a writer that came in has written
}

// Park keeps lines the state directory refused, in the file name.jsonl, until Unpark hands
// them on. A git hook in an agent's sandbox may write the repository's own git directory,
// where the store is, but not terma's state directory.
func (s *Store) Park(name string, lines []byte) error {
	unlock, _, err := s.create()
	if err != nil {
		return err
	}
	defer unlock()
	f, err := os.OpenFile(filepath.Join(s.dir, name+parkedExt), os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return err
	}
	if _, err := f.Write(lines); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Unpark passes what Park kept under name to deliver, and drops it once deliver has taken it.
func (s *Store) Unpark(name string, deliver func(lines []byte) error) error {
	path := filepath.Join(s.dir, name+parkedExt)
	if _, err := os.Stat(path); err != nil {
		return nil // nothing parked: the common case takes no lock
	}
	defer s.lock()()
	lines, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // another hook took it first
	}
	if err != nil {
		return err
	}
	if err := deliver(lines); err != nil {
		return err
	}
	return os.Remove(path)
}

// removeTemps removes atomic-write temporary files a crash left in dir before before.
func removeTemps(dir string, before time.Time) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && strings.HasPrefix(e.Name(), config.TempPrefix) && info.ModTime().Before(before) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// prune is Prune for a caller that holds the store lock.
func (s *Store) prune(before time.Time) (int, error) {
	manifests, deltas, err := s.manifests()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range manifests {
		if m.UpdatedAt.Before(before) {
			// Its deltas go too, or they would bring the session back.
			removeAll(deltas[m.Key()])
			if err := os.Remove(s.manifestPath(m.Key())); err == nil || len(deltas[m.Key()]) > 0 {
				n++
			}
		}
	}
	// Active with no ttl reports fresh, so the cutoff is the test.
	if active, _ := s.Active(before, 0); active != nil && active.UpdatedAt.Before(before) {
		_ = s.clearActive(active.Key())
	}
	return n, nil
}

// Attribution is one session's claim on a commit.
type Attribution struct {
	SessionID string
	Tool      string
	// Files are the staged paths the session touched, never empty: a commit is a session's
	// only on the files it recorded.
	Files []string
}

// Attribute returns every session whose manifest intersects the staged files, oldest
// first. A commit whose files no session recorded is nobody's: a developer's own work
// beside an open session is not the agent's.
func Attribute(staged []string, manifests []Manifest) []Attribution {
	stagedSet := make(map[string]bool, len(staged))
	for _, f := range staged {
		if n := Normalize(f); n != "" {
			stagedSet[n] = true
		}
	}
	var out []Attribution
	for _, m := range manifests {
		var hit []string
		for f := range m.Files {
			if stagedSet[f] {
				hit = append(hit, f)
			}
		}
		if len(hit) == 0 {
			continue
		}
		slices.Sort(hit)
		out = append(out, Attribution{SessionID: m.SessionID, Tool: m.ToolLabel(), Files: hit})
	}
	return out
}

// Normalize makes a relative path comparable: slashes, no leading "./", trimmed.
func Normalize(p string) string {
	p = strings.TrimSpace(p)
	p = filepath.ToSlash(p)
	p = strings.TrimPrefix(p, "./")
	return strings.TrimSuffix(p, "/")
}

func (s *Store) manifestPath(k Key) string {
	return filepath.Join(s.dir, manifestsDir, k.Tool, k.ID+manifestExt)
}

func (s *Store) manifest(k Key) (*Manifest, error) {
	if !k.valid() {
		return nil, fmt.Errorf("invalid session %q of tool %q", k.ID, k.Tool)
	}
	var m Manifest
	err := readJSON(s.manifestPath(k), &m)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if m.Files == nil {
		m.Files = map[string]time.Time{}
	}
	return &m, nil
}

func readJSON(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// writeJSON is atomic but not synced: it runs on every tool call.
// writeJSON writes v at path, creating its directory. An unlocked writer can lose the
// directory to a Retire between the two, so a write that finds it gone makes it again.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	for range 3 {
		if err = os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
			return err
		}
		err = config.WriteFileAtomicNoSync(path, data, fileMode)
		if _, statErr := os.Stat(filepath.Dir(path)); err == nil || !errors.Is(statErr, fs.ErrNotExist) {
			return err
		}
	}
	return err
}
