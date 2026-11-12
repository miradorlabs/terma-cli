package hookrun

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// TitleMaxBytes bounds a session's name: a generated one is short, a rename or a first prompt
// has no limit.
const TitleMaxBytes = 256

// titleState keeps an unchanged name from being sent at every turn's end: a hash of the name
// last spooled, never the name, which can restate the first prompt.
type titleState struct {
	Hash string `json:"hash"`
}

// SpoolTitle spools a session's name as terma.session.title when it differs from the one last
// spooled for the session, keeping state under stateDir: read reads it under the session's
// lock, found false before the agent has named the session. The name is cut to TitleMaxBytes
// on a character's edge. attrs are the agent's own (its name, where the name was read); the
// caller has checked the consent the name travels under.
func SpoolTitle(e Env, r *Repo, stateDir, sessionID string, attrs map[string]any, read func() (title string, found bool, err error)) {
	if e.Spool == nil || !session.ValidID(sessionID) {
		return
	}
	dir := filepath.Join(e.StateDir, stateDir)
	path := filepath.Join(dir, EvidenceID(sessionID)+".json")
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	unlock, err := LockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var last titleState
	b, readErr := os.ReadFile(path)
	if readErr == nil && json.Unmarshal(b, &last) != nil {
		last = titleState{}
	}
	title, found, err := read()
	if err != nil {
		e.Logf("title: %v", err)
		return
	}
	title = TruncateBytes(title, TitleMaxBytes)
	hash := EvidenceID(title)
	if !found || hash == last.Hash {
		return
	}
	out := maps.Clone(attrs)
	out[semconv.TermaSessionTitleKey], out[AttrProjectID] = title, r.ProjectID
	if err := e.Spool.Append(spool.Event{Time: e.Time(), Name: semconv.TermaSessionTitleEvent, SessionID: sessionID, Repository: r.Repository, Global: e.Policy.Global(), Attrs: out}); err != nil {
		e.Logf("title: %v", err)
		return
	}
	if b, err := json.Marshal(titleState{Hash: hash}); err == nil {
		if err := WriteState(path, b); err != nil {
			e.Logf("title state: %v", err)
		}
	}
	if os.IsNotExist(readErr) {
		PruneState(dir, e.Time().Add(-spool.MaxAge))
	}
}

// TruncateBytes cuts s to at most n bytes on a rune boundary.
func TruncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
