package omp

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/session"
)

// sameParentSessionPath reports whether headerParent names parentPath (the parent transcript
// corresponding to subPath's containing directory), matching terma-omp.ts's parentOf rule
// while tolerating slash/case normalization on Windows and symlinked session roots.
func sameParentSessionPath(subPath, parentPath, headerParent string, windows bool) bool {
	if strings.TrimSpace(headerParent) == "" || parentPath == "" {
		return false
	}
	norm := func(p string) string {
		if windows {
			p = strings.ReplaceAll(p, `\`, "/")
		}
		p = filepath.ToSlash(filepath.Clean(p))
		if windows {
			p = strings.ToLower(p)
		}
		return p
	}
	expected := norm(parentPath)
	if norm(headerParent) == expected {
		return true
	}
	if !windows {
		subDirReal, err1 := filepath.EvalSymlinks(filepath.Dir(subPath))
		hdrDirReal, err2 := filepath.EvalSymlinks(filepath.Dir(headerParent))
		if err1 == nil && err2 == nil && norm(subDirReal+".jsonl") == norm(filepath.Join(hdrDirReal, filepath.Base(headerParent))) {
			return true
		}
	}
	return false
}

// matchesParentSession reports whether a child transcript's header.parentSession links it to
// the enclosing parent transcript — either by file path (task subagents) or by bare parent
// session ID (OMP's /tan clones created via SessionManager.forkFrom).
func matchesParentSession(subPath, parentPath, parentID, headerParent string, windows bool) bool {
	if sameParentSessionPath(subPath, parentPath, headerParent, windows) {
		return true
	}
	return parentID != "" && strings.TrimSpace(headerParent) == parentID
}

// ompParentSessionID extracts a parent session ID from header.parentSession or
// header.continuationOf (a standard transcript filename/path, a bare session ID, or an
// existing regular transcript file on disk). A non-empty reference that cannot be resolved
// returns a deterministic sentinel so collectFamily fails closed rather than dropping ancestry.
func ompParentSessionID(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if id := ompSessionIDFromFilename(filepath.Base(strings.ReplaceAll(ref, `\`, "/"))); id != "" {
		return id
	}
	if session.ValidID(ref) {
		return ref
	}
	if filepath.IsAbs(ref) {
		if fi, err := os.Lstat(ref); err == nil && fi.Mode().IsRegular() {
			if hdr, err := peekOmpSessionHeader(ref); err == nil && hdr.Type == "session" && session.ValidID(hdr.ID) {
				return hdr.ID
			}
		}
	}
	sum := sha256.Sum256([]byte(ref))
	return "missing-parent-" + hex.EncodeToString(sum[:8])
}

func sortPathsParentFirst(paths []string) {
	depth := func(p string) int { return strings.Count(filepath.ToSlash(filepath.Clean(p)), "/") }
	slices.SortFunc(paths, func(a, b string) int {
		return cmp.Or(cmp.Compare(depth(a), depth(b)), strings.Compare(a, b))
	})
}

func ompSessionIDFromFilename(base string) string {
	m := ompFilenameRe.FindStringSubmatch(base)
	if len(m) == 2 && session.ValidID(m[1]) {
		return m[1]
	}
	return ""
}

// dirHasJSONLOrErr reports whether dir (or any nested subdirectory up to depth 8) holds a
// surviving .jsonl transcript or cannot be read, so a deleted intermediate subagent with
// surviving grandchildren fails closed.
func dirHasJSONLOrErr(ctx context.Context, dir string, depth int) bool {
	if depth > 8 || ctx.Err() != nil {
		return true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true
	}
	for _, ent := range entries {
		if ctx.Err() != nil {
			return true
		}
		if ent.Type()&os.ModeSymlink != 0 {
			continue
		}
		if ent.IsDir() {
			if dirHasJSONLOrErr(ctx, filepath.Join(dir, ent.Name()), depth+1) {
				return true
			}
			continue
		}
		if strings.HasSuffix(ent.Name(), ".jsonl") && !isOmpChangesJournal(filepath.Join(dir, ent.Name()), ent.Name()) {
			return true
		}
	}
	return false
}

// isOmpChangesJournal reports whether path is OMP's non-session commit/selective change
// journal (<session-stem>/changes.jsonl). It requires the exact lowercase filename on
// case-sensitive OSes and checks the leading 512 bytes so a task subagent named Changes,
// an empty newly-created file, or any session transcript is never exempted while oversized
// journal lines (>1 MiB) still match safely.
func isOmpChangesJournal(path, name string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		if !strings.EqualFold(name, "changes.jsonl") {
			return false
		}
	} else if name != "changes.jsonl" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var buf [512]byte
	n, err := f.Read(buf[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	head := bytes.TrimSpace(buf[:n])
	if len(head) == 0 {
		return false
	}
	if nl := bytes.IndexByte(head, '\n'); nl >= 0 {
		head = bytes.TrimSpace(head[:nl])
	}
	var kind struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(head, &kind) == nil && kind.Type != "" {
		return kind.Type == "change" || kind.Type == "commit"
	}
	return bytes.Contains(head, []byte(`"type":"change"`)) || bytes.Contains(head, []byte(`"type": "change"`)) ||
		bytes.Contains(head, []byte(`"type":"commit"`)) || bytes.Contains(head, []byte(`"type": "commit"`))
}
