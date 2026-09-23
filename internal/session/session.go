// Package session keeps the per-repository record of which agent sessions touched
// which files, so a commit can be attributed to the sessions that actually produced
// its staged content.
//
// State lives under <gitdir>/terma/ — inside the repository's own metadata, never in
// the worktree, so nothing here can be committed by accident and every worktree has
// its own. Two things are recorded:
//
//   - the active session (session.json): what the harness most recently announced,
//     used only as a fallback when no manifest matches;
//   - one manifest per session (manifests/<id>.json): the repo-relative files the
//     session edited, maintained as the agent works.
//
// Attribution is decided by the manifests, not by "whatever is active now": a human
// committing agent work hours later is still stamped correctly, and pure human work
// in a repo with an idle session is not.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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

// Manifest is the set of files one session edited, with the last time each was
// touched.
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
	// lockWait bounds how long a writer waits for the store lock before it goes ahead
	// without it; see lock. Open sets the hook's bound, and the tests of mutual
	// exclusion raise it, because how long a loaded machine takes is not what they test.
	lockWait time.Duration
}

const (
	stateDirName = "terma"
	activeFile   = "session.json"
	manifestsDir = "manifests"
	manifestExt  = ".json"
	lockFile     = "store.lock"
	dirMode      = 0o700
	fileMode     = 0o600
	maxIDLen     = 128
)

// Open addresses the state under gitDir without creating anything.
func Open(gitDir string) *Store {
	return &Store{dir: filepath.Join(gitDir, stateDirName), lockWait: hookLockWait}
}

// safeID is what a session id is allowed to look like when it becomes a file name.
var safeID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidID reports whether id is safe to persist and to write into a commit message.
func ValidID(id string) bool {
	return id != "" && len(id) <= maxIDLen && safeID.MatchString(id) && !strings.HasPrefix(id, ".")
}

// isMultiline reports whether a value would break out of the single line a trailer
// occupies. Checked on the values that are not identifiers and so cannot be held to
// ValidID's charset.
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
	// Under the store lock, like every other writer of this file: Touch refreshes the
	// active session by reading it and writing it back, and an announcement landing
	// between the two was overwritten by the session it had just replaced.
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
	defer s.lock()()
	return s.clearActive(id)
}

// clearActive is ClearActive for a caller that already holds the store lock. Its check
// and its remove are one step only under that lock: unlocked, a Touch could rewrite the
// record in between and have it deleted from under it.
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

// hookLockWait bounds how long a hook waits for the store. Holders rewrite a few
// kilobytes, so a wait that long means something is wrong, and a hook must not hang on it.
const hookLockWait = 250 * time.Millisecond

// lock serializes the store's read-modify-write paths. Hooks run concurrently — Codex's
// PostToolUse is asynchronous, and a subagent's edits arrive under its parent's session
// id — and each of them reads a manifest, adds to it and renames it back: unlocked, the
// second rename dropped the first one's files, and the commit that carried them went
// out unattributed.
//
// It covers every writer of the store's two kinds of file — the manifests (Touch,
// Consume, Prune) and the active session (SetActive, ClearActive, and Touch's refresh).
// Readers take nothing: every write is an atomic rename.
//
// One lock for the store rather than one per manifest, so there is nothing to prune. It
// is never taken twice in one call path: a second flock on another descriptor waits on
// the first, even in the same process — which is why Prune calls clearActive, not
// ClearActive. A lock that cannot be had — no state directory
// yet, or a holder that outlasts the store's lockWait — returns a no-op: the write goes ahead as it
// always did, which can lose a race but never loses the write.
func (s *Store) lock() (unlock func()) {
	ctx, cancel := context.WithTimeout(context.Background(), s.lockWait)
	defer cancel()
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, lockFile))
	if err != nil {
		return func() {}
	}
	return unlock
}

// Touch records that session sessionID edited files (repo-relative, slash-separated)
// at time at. Unknown sessions get a manifest; the active session's UpdatedAt is
// refreshed so the TTL fallback tracks real activity, not just the start.
func (s *Store) Touch(sess Session, files []string, at time.Time) error {
	if !ValidID(sess.ID) {
		return fmt.Errorf("invalid session id %q", sess.ID)
	}
	// Touch is what creates the state, so the directory has to exist to be locked.
	if err := os.MkdirAll(s.dir, dirMode); err != nil {
		return err
	}
	defer s.lock()()
	m, err := s.manifest(sess.ID)
	if err != nil {
		return err
	}
	if m == nil {
		m = &Manifest{
			SessionID:   sess.ID,
			Tool:        sess.Tool,
			ToolVersion: sess.ToolVersion,
			StartedAt:   at,
			Files:       map[string]time.Time{},
		}
	}
	if m.Tool == "" {
		m.Tool, m.ToolVersion = sess.Tool, sess.ToolVersion
	}
	for _, f := range files {
		f = Normalize(f)
		if f == "" {
			continue
		}
		m.Files[f] = at
	}
	m.UpdatedAt = at
	if err := writeJSON(s.manifestPath(sess.ID), m); err != nil {
		return err
	}
	// Keep the active session's freshness in step with real activity.
	if active, _ := s.Active(at, 0); active != nil && active.ID == sess.ID {
		active.UpdatedAt = at
		_ = writeJSON(filepath.Join(s.dir, activeFile), active)
	}
	return nil
}

// Manifests lists every recorded session, oldest first.
func (s *Store) Manifests() ([]Manifest, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, manifestsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Manifest
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), manifestExt) {
			continue
		}
		var m Manifest
		if err := readJSON(filepath.Join(s.dir, manifestsDir, e.Name()), &m); err != nil {
			continue // a torn write is skipped, never fatal to a commit
		}
		// Every write path checks ValidID, but the check has to be repeated on read:
		// these are files, and what is on disk is not necessarily what this code put
		// there. The id read back flows into a commit message (a newline in it would
		// forge trailer lines git and the GitHub App then parse as real) and into
		// manifestPath, where Prune removes it — an id of "../../x" would delete
		// outside this directory. Requiring the id to be exactly the file name it was
		// read from enforces the invariant writeJSON establishes, so manifestPath is
		// guaranteed to address the file this manifest actually came from.
		if !ValidID(m.SessionID) || e.Name() != m.SessionID+manifestExt {
			continue
		}
		// Tool and ToolVersion are rendered into the same commit message and are not
		// identifiers, so they get the weaker rule: no line breaks, ever.
		if isMultiline(m.Tool) || isMultiline(m.ToolVersion) {
			m.Tool, m.ToolVersion = "", ""
		}
		if m.Files == nil {
			m.Files = map[string]time.Time{}
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, nil
}

// Consume removes files from a session's manifest once a commit has carried them,
// so the next commit is not attributed to work that already shipped. An emptied
// manifest is kept (with no files): it is still the evidence that this session
// reports its edits, which keeps the active-session fallback from claiming a
// later human commit. Prune retires it with the rest.
func (s *Store) Consume(sessionID string, files []string) error {
	defer s.lock()()
	m, err := s.manifest(sessionID)
	if err != nil || m == nil {
		return err
	}
	for _, f := range files {
		delete(m.Files, Normalize(f))
	}
	return writeJSON(s.manifestPath(sessionID), m)
}

// Merge folds one session's manifest into another's and retires the first. It is how a
// subagent that edited under an id of its own is attributed to the conversation that
// spawned it: Cursor can file a subagent's afterFileEdit under the subagent's
// conversation id, and left alone the commit would be stamped with a session nobody can
// find, or with two.
//
// Each file keeps its own touch time, the later one where both sessions touched it. The
// target keeps its tool, or takes the source's when it is new. An active record for the
// source is cleared: a session folded away must not go on claiming commits through the
// fallback. A source with no manifest merges nothing. Returns the files that moved,
// sorted.
func (s *Store) Merge(fromID string, into Session, at time.Time) ([]string, error) {
	if !ValidID(into.ID) {
		return nil, fmt.Errorf("invalid session id %q", into.ID)
	}
	if !ValidID(fromID) || fromID == into.ID {
		return nil, nil
	}
	defer s.lock()()
	src, err := s.manifest(fromID)
	if err != nil || src == nil {
		return nil, err
	}
	dst, err := s.manifest(into.ID)
	if err != nil {
		return nil, err
	}
	if dst == nil {
		dst = &Manifest{SessionID: into.ID, Tool: into.Tool, ToolVersion: into.ToolVersion, StartedAt: at, Files: map[string]time.Time{}}
	}
	if dst.Tool == "" {
		dst.Tool, dst.ToolVersion = src.Tool, src.ToolVersion
	}
	files := make([]string, 0, len(src.Files))
	for f, touched := range src.Files {
		if prev, ok := dst.Files[f]; !ok || touched.After(prev) {
			dst.Files[f] = touched
		}
		files = append(files, f)
	}
	if at.After(dst.UpdatedAt) {
		dst.UpdatedAt = at
	}
	// The target is written before the source goes: a hook killed between the two leaves
	// the files in both manifests, which a commit survives, rather than in neither.
	if err := writeJSON(s.manifestPath(into.ID), dst); err != nil {
		return nil, err
	}
	if err := os.Remove(s.manifestPath(fromID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	_ = s.clearActive(fromID) // the lock is already held
	sort.Strings(files)
	return files, nil
}

// Prune drops manifests not updated since before, and a stale active session.
func (s *Store) Prune(before time.Time) (int, error) {
	// Under the lock: a manifest read as stale must not be removed after a Touch has
	// just brought it back to life.
	defer s.lock()()
	manifests, err := s.Manifests()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range manifests {
		if m.UpdatedAt.Before(before) {
			if err := os.Remove(s.manifestPath(m.SessionID)); err == nil {
				n++
			}
		}
	}
	// Active with no ttl always reports fresh, so asking it for staleness meant this never
	// ran and a finished session's record stayed for ever; the cutoff is the only test.
	if active, _ := s.Active(before, 0); active != nil && active.UpdatedAt.Before(before) {
		_ = s.clearActive(active.ID) // the lock is already held
	}
	return n, nil
}

// installRecord is the per-clone memory of what `terma install` changed in git's
// configuration, so uninstall can put it back exactly.
type installRecord struct {
	PreviousHooksPath string    `json:"previous_hooks_path"`
	RecordedAt        time.Time `json:"recorded_at"`
}

const installFile = "install.json"

// RecordPreviousHooksPath remembers the core.hooksPath in force before terma
// pointed git at its shims. Recording twice keeps the first value: the second
// run would otherwise "remember" terma's own path.
func RecordPreviousHooksPath(gitDir, previous string) error {
	path := filepath.Join(gitDir, stateDirName, installFile)
	var existing installRecord
	if err := readJSON(path, &existing); err == nil && !existing.RecordedAt.IsZero() {
		return nil
	}
	return writeJSON(path, installRecord{PreviousHooksPath: previous, RecordedAt: time.Now()})
}

// PreviousHooksPath returns what RecordPreviousHooksPath stored; ok is false when
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

// Attribute decides which sessions a commit belongs to: every session whose
// manifest intersects the staged files, oldest session first.
//
// The fresh active session claims a commit only when it has no manifest at all —
// the fallback for harnesses that announce sessions but cannot report file edits.
// A session that does report edits is judged by them alone: if none of its files
// are staged, the commit is human work and stays unstamped, however recently the
// session was active.
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
		sort.Strings(hit)
		out = append(out, Attribution{SessionID: m.SessionID, Tool: m.ToolLabel(), Files: hit})
	}
	if len(out) == 0 && active != nil && activeFresh && !activeHasManifest && len(stagedSet) > 0 {
		out = append(out, Attribution{SessionID: active.ID, Tool: active.ToolLabel()})
	}
	return out
}

// Normalize makes a path comparable across the sources that produce them: slashes,
// no leading "./", trimmed. Absolute paths are the caller's job to relativize.
func Normalize(p string) string {
	p = strings.TrimSpace(p)
	p = filepath.ToSlash(p)
	p = strings.TrimPrefix(p, "./")
	return strings.TrimSuffix(p, "/")
}

// --- files ------------------------------------------------------------------

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

// writeJSON is atomic: a hook interrupted mid-write must never leave a torn file for
// the next commit to choke on. Not fsynced — this runs on every tool call, and a
// manifest lost to a power cut costs one commit its trailer, not a session its state.
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
