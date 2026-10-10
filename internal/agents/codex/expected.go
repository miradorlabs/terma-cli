package codex

import (
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// codexExpectedDir holds, per shell call, the files its PreToolUse claimed and how each
// stood before the call: a call the developer declines never runs, and gets no PostToolUse.
var codexExpectedDir = hookrun.AgentStateDir(name, "expected")

// expectedCall is one call's claims: repo-relative paths in Root, each with its state
// before the call, nil when it did not exist.
type expectedCall struct {
	Root  string                `json:"root"`
	Files map[string]*fileState `json:"files"`
}

type fileState struct {
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

func statFile(path string) *fileState {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return &fileState{Size: info.Size(), ModTime: info.ModTime()}
}

func (s *fileState) same(t *fileState) bool {
	if s == nil || t == nil {
		return s == t
	}
	return s.Size == t.Size && s.ModTime.Equal(t.ModTime)
}

// expect records sess's claim on files, which PreToolUse just added to its manifest. A file
// the manifest already held is left out: an earlier edit claimed it.
func expect(e hookrun.Env, r *hookrun.Repo, sess session.Session, held map[string]bool, files []string) {
	call := expectedCall{Root: r.Root, Files: map[string]*fileState{}}
	for _, f := range files {
		if !held[f] {
			call.Files[f] = statFile(filepath.Join(r.Root, filepath.FromSlash(f)))
		}
	}
	if len(call.Files) == 0 {
		return
	}
	dir := filepath.Join(e.StateDir, codexExpectedDir)
	b, err := json.Marshal(call)
	if err == nil {
		err = os.MkdirAll(dir, 0o700)
	}
	if err == nil {
		// One file per call: parallel calls of a session never write the same one.
		var f *os.File
		if f, err = os.CreateTemp(dir, hookrun.EvidenceID(sess.ID)+".*.json"); err == nil {
			_, err = f.Write(b)
			err = cmp.Or(err, f.Close())
		}
	}
	if err != nil {
		e.Logf("codex expected files: %v", err)
	}
}

// settleExpected, at the end of a turn, withdraws the claims of the session's calls on files
// they left as they were: the call was declined or wrote nothing there.
func settleExpected(e hookrun.Env, r *hookrun.Repo, sessionID string) {
	dir := filepath.Join(e.StateDir, codexExpectedDir)
	paths, _ := filepath.Glob(filepath.Join(dir, hookrun.EvidenceID(sessionID)+".*.json"))
	var unchanged []string
	for _, p := range paths {
		var call expectedCall
		if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &call) == nil && call.Root == r.Root {
			for f, before := range call.Files {
				if before.same(statFile(filepath.Join(r.Root, filepath.FromSlash(f)))) {
					unchanged = append(unchanged, f)
				}
			}
		}
		_ = os.Remove(p)
	}
	if len(unchanged) > 0 {
		if err := r.Store.Consume(session.Key{Tool: codexTool, ID: sessionID}, unchanged); err != nil {
			e.Logf("codex expected files: %v", err)
		}
	}
}

// heldFiles is the files sess's manifest in r holds.
func heldFiles(r *hookrun.Repo, k session.Key) map[string]bool {
	held := map[string]bool{}
	manifests, _ := r.Store.Manifests()
	for _, m := range manifests {
		if m.Key() == k {
			for f := range m.Files {
				held[f] = true
			}
		}
	}
	return held
}
