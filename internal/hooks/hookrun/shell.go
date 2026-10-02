package hookrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// A shell tool call names no files, so its edits are read off the working tree: git's
// changed files before the call, fingerprinted, against the same after it. A file that
// changed in between is the call's. The window is the call's own, so an edit someone else
// makes in this checkout while it runs is the call's too, except a file another session's
// manifest recorded in that window: that session's edit tool wrote it.

// maxShellFiles bounds a snapshot: a tree with more changed files than this is build
// output, and diffing it on every shell call would cost more than it tells.
const maxShellFiles = 2000

// AttrEditSource says how a files.touched event learned its files when an edit tool did
// not name them.
const AttrEditSource = "edit_source"

// ShellBefore snapshots the checkout's changed files before a shell call runs; callID is
// the call's id, which its ShellAfter repeats. A call with no id is skipped: two of them
// in one session would share a snapshot, and one would be diffed against the other's start.
func (e Env) ShellBefore(ctx context.Context, r *Repo, sessionID, callID string) {
	if r.GitDir == "" || callID == "" || !session.ValidID(sessionID) {
		return
	}
	changed, err := gitx.ChangedFiles(ctx, r.Root)
	if err != nil || len(changed) > maxShellFiles {
		e.Logf("shell snapshot skipped: %d files, %v", len(changed), err)
		return
	}
	snap := session.ShellSnapshot{SessionID: sessionID, At: e.Time(), Files: make(map[string]string, len(changed))}
	for _, f := range changed {
		snap.Files[f] = fingerprint(r.Root, f)
	}
	if err := r.Store.SaveShell(shellKey(sessionID, callID), snap); err != nil {
		e.Logf("shell snapshot: %v", err)
	}
}

// ShellAfter adds to sess's manifest the files its shell call changed, if ShellBefore
// saw the call start.
func (e Env) ShellAfter(ctx context.Context, r *Repo, sess session.Session, toolName, callID string, extra map[string]any) {
	if r.GitDir == "" || callID == "" || !session.ValidID(sess.ID) {
		return
	}
	snap, ok := r.Store.TakeShell(shellKey(sess.ID, callID))
	if !ok || snap.SessionID != sess.ID {
		return
	}
	changed, err := gitx.ChangedFiles(ctx, r.Root)
	if err != nil || len(changed) > maxShellFiles {
		e.Logf("shell diff skipped: %d files, %v", len(changed), err)
		return
	}
	others := othersTouchedSince(r.Store, sess.ID, snap)
	var files []string
	for _, f := range changed {
		if before, seen := snap.Files[f]; seen && before == fingerprint(r.Root, f) || others[session.Normalize(f)] {
			continue
		}
		files = append(files, f)
	}
	if extra == nil {
		extra = map[string]any{}
	}
	extra[AttrEditSource] = "shell"
	e.Touch(r, sess, toolName, UniqueSorted(files), extra)
}

// othersTouchedSince is the files another session's edit tools recorded since snap.
func othersTouchedSince(store *session.Store, id string, snap session.ShellSnapshot) map[string]bool {
	manifests, _ := store.Manifests()
	out := map[string]bool{}
	for _, m := range manifests {
		if m.SessionID == id {
			continue
		}
		for f, at := range m.Files {
			if !at.Before(snap.At) {
				out[f] = true
			}
		}
	}
	return out
}

// fingerprint is a file's state as cheaply as it can be read: a write changes its
// modification time, a deletion its existence.
func fingerprint(root, rel string) string {
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "-"
	}
	return fmt.Sprintf("%d:%d:%o", info.ModTime().UnixNano(), info.Size(), info.Mode())
}

// shellKey names a call's snapshot by its session and id, both from the payload.
func shellKey(sessionID, callID string) string {
	return EvidenceID(sessionID + "\x00" + callID)[:32]
}
