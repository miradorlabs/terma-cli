package shellwrites

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// Claims keeps, per shell call, the files its PreToolUse claimed and how each stood before
// the call, under Dir in the hook state directory: a call the developer declines never runs,
// and gets no PostToolUse. Tool is the agent's session tool, whose manifests they are.
type Claims struct {
	Dir  string
	Tool string
}

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

// BeforeCall records in sess's manifest the files a shell command writes before its first
// commit, so that commit is stamped: the call's PostToolUse comes after it. A write between
// two commits is left out, since it would claim the first. A call that makes no commit has
// all its files recorded. The turn's Stop withdraws the claims on files the call left
// unchanged (Settle).
func (c Claims) BeforeCall(ctx context.Context, e hookrun.Env, sess session.Session, command string) {
	paths, commits := Writes(command, e.Cwd)
	if len(commits) > 0 {
		paths = paths[:commits[0]] // a write after a commit must not claim it
	}
	if len(paths) == 0 {
		return
	}
	r, err := e.Repo(ctx)
	if err != nil {
		return
	}
	owned := held(r, sess.Key())
	c.expect(e, r, sess, owned, e.Expect(r, sess, paths))
}

// Settle withdraws, at the end of a turn, the claims of the session's calls on files they
// left as they were: the call was declined or wrote nothing there. A file two parallel calls
// claimed is withdrawn only when neither changed it.
func (c Claims) Settle(e hookrun.Env, r *hookrun.Repo, sessionID string) {
	paths := c.calls(e, sessionID)
	same := map[string]bool{}
	for _, p := range paths {
		var call expectedCall
		b, err := os.ReadFile(p)
		if err == nil && json.Unmarshal(b, &call) == nil && call.Root != r.Root {
			continue // another checkout's: its own Stop settles it
		}
		for f, before := range call.Files {
			unchanged := before.same(statFile(filepath.Join(r.Root, filepath.FromSlash(f))))
			if prior, seen := same[f]; !seen || prior {
				same[f] = unchanged
			}
		}
		_ = os.Remove(p)
	}
	var unchanged []string
	for f, ok := range same {
		if ok {
			unchanged = append(unchanged, f)
		}
	}
	if len(unchanged) > 0 {
		if err := r.Store.Consume(session.Key{Tool: c.Tool, ID: sessionID}, unchanged); err != nil {
			e.Logf("expected files: %v", err)
		}
	}
}

// Pending reports whether the session has claims to settle, without resolving its repository.
func (c Claims) Pending(e hookrun.Env, sessionID string) bool { return len(c.calls(e, sessionID)) > 0 }

// calls are the files of the session's claimed calls.
func (c Claims) calls(e hookrun.Env, sessionID string) []string {
	paths, _ := filepath.Glob(filepath.Join(e.StateDir, c.Dir, hookrun.EvidenceID(sessionID)+".*.json"))
	return paths
}

// expect records sess's claim on files, which BeforeCall just added to its manifest. A file
// the manifest already held is left out: an earlier edit claimed it.
func (c Claims) expect(e hookrun.Env, r *hookrun.Repo, sess session.Session, held map[string]bool, files []string) {
	call := expectedCall{Root: r.Root, Files: map[string]*fileState{}}
	for _, f := range files {
		if !held[f] {
			call.Files[f] = statFile(filepath.Join(r.Root, filepath.FromSlash(f)))
		}
	}
	if len(call.Files) == 0 {
		return
	}
	dir := filepath.Join(e.StateDir, c.Dir)
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
		e.Logf("expected files: %v", err)
	}
}

func (s *fileState) same(t *fileState) bool {
	if s == nil || t == nil {
		return s == t
	}
	return s.Size == t.Size && s.ModTime.Equal(t.ModTime)
}

// AfterCall reports the files a shell command wrote and records in sess's manifest those its
// commits did not take in. A shell command's targets are guesses, written or not: only the
// session's own manifest records them, where the turn's Stop withdraws the unchanged ones.
func AfterCall(e hookrun.Env, r *hookrun.Repo, sess session.Session, toolName, command string, attrs map[string]any) {
	candidates, commits := Writes(command, e.Cwd)
	if len(candidates) == 0 {
		return
	}
	recorded := candidates
	if len(commits) > 0 {
		// The call's commits took in what it wrote before them: recording those files again
		// would hand the next commit of them to this session.
		recorded = candidates[commits[len(commits)-1]:]
	}
	e.Expect(r, sess, recorded)
	e.Report(r, sess, toolName, candidates, attrs)
}

// held is the files k's manifest in r holds.
func held(r *hookrun.Repo, k session.Key) map[string]bool {
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

func statFile(path string) *fileState {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return &fileState{Size: info.Size(), ModTime: info.ModTime()}
}
