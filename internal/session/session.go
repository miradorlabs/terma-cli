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
	stateDirName = "terma"
	activeFile   = "session.json"
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

// Open addresses state under its Git or private storage root without creating it.
func Open(stateRoot string) *Store {
	return &Store{dir: filepath.Join(stateRoot, stateDirName), lockWait: hookLockWait}
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
	if !ValidID(sess.ID) {
		return fmt.Errorf("invalid session id %q", sess.ID)
	}
	if sess.StartedAt.IsZero() {
		sess.StartedAt = time.Now()
	}
	if sess.UpdatedAt.IsZero() {
		sess.UpdatedAt = sess.StartedAt
	}
	// Locked because Touch reads and rewrites the active session.
	if err := os.MkdirAll(s.dir, dirMode); err != nil {
		return err
	}
	defer s.lock()()
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

// ClearActive forgets the active session if it is id (or any, when id is empty).
func (s *Store) ClearActive(id string) error {
	if s.missing() {
		return nil
	}
	defer s.lock()()
	return s.clearActive(id)
}

// clearActive is ClearActive for a caller that already holds the store lock.
func (s *Store) clearActive(id string) error {
	path := filepath.Join(s.dir, activeFile)
	if id != "" {
		var sess Session
		if err := readJSON(path, &sess); err != nil || sess.ID != id {
			return nil
		}
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
// no store there is nothing to change. Only uninstall's Remove deletes a store, unlocked.
func (s *Store) missing() bool {
	_, err := os.Stat(s.dir)
	return errors.Is(err, fs.ErrNotExist)
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
	if !ValidID(sess.ID) {
		return fmt.Errorf("invalid session id %q", sess.ID)
	}
	// Touch creates the state, so the directory must exist to be locked.
	if err := os.MkdirAll(s.dir, dirMode); err != nil {
		return err
	}
	unlock, held := s.acquire()
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
	m, deltas, err := s.load(sess.ID)
	if err != nil {
		return err
	}
	m = fold(m, touched)
	if err := writeJSON(s.manifestPath(sess.ID), m); err != nil {
		return err
	}
	removeAll(deltas)
	if active, _ := s.Active(at, 0); active != nil && active.ID == sess.ID {
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
	if dst.Tool == "" {
		dst.Tool, dst.ToolVersion = src.Tool, src.ToolVersion
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
	return writeJSON(filepath.Join(s.dir, manifestsDir, m.SessionID+deltaSep+hex.EncodeToString(b[:])+deltaExt), m)
}

// load is id's manifest with its deltas folded in, and the deltas' paths, which a locked
// writer removes once it has written the result. A delta that appears later is kept.
func (s *Store) load(id string) (*Manifest, []string, error) {
	m, err := s.manifest(id)
	if err != nil {
		return nil, nil, err
	}
	paths, _ := filepath.Glob(filepath.Join(s.dir, manifestsDir, id+deltaSep+"*"+deltaExt))
	var folded []string
	for _, p := range paths {
		var d Manifest
		if readJSON(p, &d) != nil || d.SessionID != id {
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

// manifests is Manifests, with each session's delta paths.
func (s *Store) manifests() ([]Manifest, map[string][]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, manifestsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	byID := map[string]*Manifest{}
	deltas := map[string][]string{}
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
		if err := readJSON(filepath.Join(s.dir, manifestsDir, name), &m); err != nil {
			continue // a torn write is skipped, never fatal to a commit
		}
		// Revalidate on read: the id reaches a commit message (a newline forges trailers)
		// and Prune's manifestPath ("../../x" deletes elsewhere), so it must match its file name.
		if !ValidID(m.SessionID) || id != m.SessionID {
			continue
		}
		if m.Files == nil {
			m.Files = map[string]time.Time{}
		}
		if strings.HasSuffix(name, deltaExt) {
			deltas[id] = append(deltas[id], filepath.Join(s.dir, manifestsDir, name))
		}
		byID[id] = fold(byID[id], &m)
	}
	out := make([]Manifest, 0, len(byID))
	for _, m := range byID {
		// Tool and ToolVersion reach the commit message too: no line breaks.
		if isMultiline(m.Tool) || isMultiline(m.ToolVersion) {
			m.Tool, m.ToolVersion = "", ""
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, deltas, nil
}

// Consume removes files a commit carried from a session's manifest; an emptied manifest
// is kept as evidence the session reports edits, so the fallback never claims for it.
func (s *Store) Consume(sessionID string, files []string) error {
	if s.missing() {
		return nil
	}
	defer s.lock()()
	m, deltas, err := s.load(sessionID)
	if err != nil || m == nil {
		return err
	}
	for _, f := range files {
		delete(m.Files, Normalize(f))
	}
	if err := writeJSON(s.manifestPath(sessionID), m); err != nil {
		return err
	}
	removeAll(deltas)
	return nil
}

// Merge folds fromID's manifest into into's (later touch wins), retires it and its
// active record, and returns the files that moved, sorted.
func (s *Store) Merge(fromID string, into Session, at time.Time) ([]string, error) {
	if !ValidID(into.ID) {
		return nil, fmt.Errorf("invalid session id %q", into.ID)
	}
	if !ValidID(fromID) || fromID == into.ID || s.missing() {
		return nil, nil
	}
	defer s.lock()()
	src, srcDeltas, err := s.load(fromID)
	if err != nil || src == nil {
		return nil, err
	}
	dst, dstDeltas, err := s.load(into.ID)
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
	if err := writeJSON(s.manifestPath(into.ID), dst); err != nil {
		return nil, err
	}
	removeAll(dstDeltas)
	if err := os.Remove(s.manifestPath(fromID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	removeAll(srcDeltas)
	_ = s.clearActive(fromID)
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
	manifests, deltas, err := s.manifests()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range manifests {
		if m.UpdatedAt.Before(before) {
			// Its deltas go too, or they would bring the session back.
			removeAll(deltas[m.SessionID])
			if err := os.Remove(s.manifestPath(m.SessionID)); err == nil || len(deltas[m.SessionID]) > 0 {
				n++
			}
		}
	}
	// Active with no ttl reports fresh, so the cutoff is the test.
	if active, _ := s.Active(before, 0); active != nil && active.UpdatedAt.Before(before) {
		_ = s.clearActive(active.ID)
	}
	return n, nil
}

// installRecord is what `terma install` changed in git's configuration, for uninstall.
type installRecord struct {
	PreviousHooksPath string    `json:"previous_hooks_path"`
	HooksConfigScope  string    `json:"hooks_config_scope,omitempty"`
	HooksPathLocal    *bool     `json:"hooks_path_local,omitempty"`
	RecordedAt        time.Time `json:"recorded_at"`
}

const installFile = "install.json"

// RecordPreviousHooksPathAtScope records the previous hooks path, its git config scope,
// and whether it was set explicitly there.
func RecordPreviousHooksPathAtScope(gitDir, previous, scope string, local bool, chainPath string) error {
	path := filepath.Join(gitDir, stateDirName, installFile)
	if err := writeJSON(path, installRecord{PreviousHooksPath: previous, HooksConfigScope: scope, HooksPathLocal: &local, RecordedAt: time.Now()}); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(gitDir, stateDirName, "previous-hooks-path"), []byte(chainPath+"\n"), fileMode)
}

// PreviousHooksScope returns the git config scope install changed.
func PreviousHooksScope(gitDir string) string {
	var rec installRecord
	if readJSON(filepath.Join(gitDir, stateDirName, installFile), &rec) == nil && rec.HooksConfigScope == "--worktree" {
		return "--worktree"
	}
	return "--local"
}

// PreviousHooksPath returns the saved original hook path; ok is false when
// nothing was recorded.
func PreviousHooksPath(gitDir string) (previous string, ok bool) {
	var rec installRecord
	if err := readJSON(filepath.Join(gitDir, stateDirName, installFile), &rec); err != nil {
		return "", false
	}
	return rec.PreviousHooksPath, true
}

// Remove deletes all state (uninstall).
func (s *Store) Remove() error {
	err := os.RemoveAll(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Attribution is one session's claim on a commit.
type Attribution struct {
	SessionID string
	Tool      string
	// Files are the staged paths the session touched (empty for the TTL fallback).
	Files []string
}

// Attribute returns every session whose manifest intersects the staged files, oldest
// first; the fresh active session claims a commit only when it has no manifest at all.
func Attribute(staged []string, manifests []Manifest, active *Session, activeFresh bool) []Attribution {
	stagedSet := make(map[string]bool, len(staged))
	for _, f := range staged {
		if n := Normalize(f); n != "" {
			stagedSet[n] = true
		}
	}
	var out []Attribution
	activeHasManifest := false
	for _, m := range manifests {
		if active != nil && m.SessionID == active.ID {
			activeHasManifest = true
		}
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
	if len(out) == 0 && active != nil && activeFresh && !activeHasManifest && len(stagedSet) > 0 {
		out = append(out, Attribution{SessionID: active.ID, Tool: active.ToolLabel()})
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

func (s *Store) manifestPath(id string) string {
	return filepath.Join(s.dir, manifestsDir, id+manifestExt)
}

func (s *Store) manifest(id string) (*Manifest, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("invalid session id %q", id)
	}
	var m Manifest
	err := readJSON(s.manifestPath(id), &m)
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
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomicNoSync(path, data, fileMode)
}

// HooksPathWasLocal reports whether the previous hooksPath was set explicitly rather
// than inherited, so uninstall restores inheritance rather than pinning it.
func HooksPathWasLocal(gitDir string) bool {
	var rec installRecord
	if err := readJSON(filepath.Join(gitDir, stateDirName, installFile), &rec); err != nil {
		return false
	}
	if rec.HooksPathLocal != nil {
		return *rec.HooksPathLocal
	}
	return rec.PreviousHooksPath != ""
}
