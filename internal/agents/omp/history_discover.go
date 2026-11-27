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
	// A sessions folder that moved (another machine, a renamed home, a changed
	// PI_CODING_AGENT_DIR) leaves each subagent's header naming its parent where it used to
	// be. It is the same parent when the paths agree from the top-level session's transcript
	// name down: that name carries the session's start time and ID, which the subagents OMP
	// copies into a fork's folder do not share with the fork.
	if tail := ompSessionTail(parentPath, windows); tail != "" && strings.HasSuffix(norm(headerParent), "/"+norm(tail)) {
		return true
	}
	return false
}

// ompSessionTail is parentPath from its top-level session's transcript (or that transcript's
// folder) down, "<time>_<id>.jsonl" or "<time>_<id>/0-explore.jsonl", or "" when no part of
// it is named like a top-level transcript.
func ompSessionTail(parentPath string, windows bool) string {
	p := parentPath
	if windows {
		p = strings.ReplaceAll(p, `\`, "/")
	}
	parts := strings.Split(filepath.ToSlash(filepath.Clean(p)), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		name := parts[i]
		if i < len(parts)-1 {
			name += ".jsonl"
		}
		if ompFilenameRe.MatchString(name) {
			return strings.Join(parts[i:], "/")
		}
	}
	return ""
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

// errOmpHeaderTooLarge is peekOmpSessionHeader's error for a file whose leading lines pass
// 1 MiB without a session header.
var errOmpHeaderTooLarge = errors.New("omp session header exceeds 1 MiB")

// ompLocalDir is the folder of a session's (or subagent's) own folder that OMP keeps its
// local:// files in: artifacts the agent wrote, such as local://results.jsonl, not transcripts.
const ompLocalDir = "local"

// isOmpLocalDir reports whether name, a folder beside the transcripts named in stems, is OMP's
// local:// folder. A task the model named "local" has a local.jsonl beside its folder.
func isOmpLocalDir(name string, stems map[string]bool) bool {
	return name == ompLocalDir && !stems[ompLocalDir]
}

// dirHasJSONLOrErr reports whether dir (or any nested subdirectory up to depth 8) holds a
// surviving .jsonl transcript or cannot be read, so a deleted intermediate subagent with
// surviving grandchildren fails closed.
func dirHasJSONLOrErr(ctx context.Context, dir string, depth int) bool {
	return scanForTranscripts(ctx, dir, depth, false)
}

// scanForTranscripts is dirHasJSONLOrErr. Under a local:// folder (inLocal) a .jsonl file is
// an artifact unless it opens as an OMP session transcript or cannot be read.
func scanForTranscripts(ctx context.Context, dir string, depth int, inLocal bool) bool {
	if depth > 8 || ctx.Err() != nil {
		return true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true
	}
	stems := map[string]bool{}
	for _, ent := range entries {
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".jsonl") {
			stems[strings.TrimSuffix(ent.Name(), ".jsonl")] = true
		}
	}
	for _, ent := range entries {
		if ctx.Err() != nil {
			return true
		}
		if ent.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(dir, ent.Name())
		if ent.IsDir() {
			if scanForTranscripts(ctx, path, depth+1, inLocal || isOmpLocalDir(ent.Name(), stems)) {
				return true
			}
			continue
		}
		if !strings.HasSuffix(ent.Name(), ".jsonl") {
			continue
		}
		if inLocal {
			if hdr, err := peekOmpSessionHeader(path); hdr.Type == "session" || err != nil && !errors.Is(err, errOmpHeaderTooLarge) {
				return true
			}
			continue
		}
		if !isOmpChangesJournal(path, ent.Name()) {
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
