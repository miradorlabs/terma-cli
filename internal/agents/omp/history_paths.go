package omp

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"unicode"

	"github.com/miradorlabs/terma-cli/internal/session"
)

var (
	ompInternalURLRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)
	ompLineRangeRe   = regexp.MustCompile(`(?::\d+(?:-\d+)?|#L?\d+(?:-L?\d+)?)$`)
)

// nonURLPath converts local file:// URLs to filesystem paths and filters out non-filesystem
// OMP internal URLs (agent://, cfg://, local://, http://, etc.).
func nonURLPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if len(p) >= 7 && strings.EqualFold(p[:7], "file://") {
		u, err := url.Parse(p)
		if err != nil || u.Path == "" {
			return ""
		}
		if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
			return ""
		}
		decoded := u.Path
		if len(decoded) >= 3 && decoded[0] == '/' && isASCIILetter(decoded[1]) && decoded[2] == ':' {
			decoded = decoded[1:]
		}
		return decoded
	}
	if ompInternalURLRe.MatchString(p) {
		return ""
	}
	return p
}

func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// stripPathSelector peels an optional trailing line selector (e.g. :1-2, :10, #L1-L2) while
// preserving Windows drive colons (C:).
func stripPathSelector(p string) string {
	loc := ompLineRangeRe.FindStringIndex(p)
	if loc == nil || loc[0] <= 1 {
		return p
	}
	return p[:loc[0]]
}

// unwrapHashlinePath strips an optional [PATH] or [PATH#ABCD] hashline header wrapper,
// matching adapter/omp/tool_trace.go's unwrapHashlinePath.
func unwrapHashlinePath(p string) string {
	inner, opened := strings.CutPrefix(strings.TrimRightFunc(p, unicode.IsSpace), "[")
	inner, closed := strings.CutSuffix(inner, "]")
	if !opened || !closed {
		return p
	}
	if loc := ompHashlineTagRe.FindStringIndex(inner); loc != nil {
		inner = inner[:loc[0]]
	} else if strings.Contains(inner, "#") {
		return p
	}
	return cmp.Or(inner, p)
}

func isAbsToolPath(p string) bool {
	if filepath.IsAbs(p) {
		return true
	}
	return len(p) >= 3 && isASCIILetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

// resolveReachPath decodes local file:// URLs, filters internal URLs, and expands a leading
// ~ / ~/ / ~\ to the user's home directory so reach classification never joins ~/... under
// the session cwd. An unexpandable ~ path (missing home dir or ~otheruser/...) returns
// unknown=true to fail closed.
func resolveReachPath(raw string) (string, bool) {
	p := nonURLPath(raw)
	if p == "" {
		return "", false
	}
	if !strings.HasPrefix(p, "~") {
		return p, false
	}
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p, true
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p, true
	}
	if p == "~" {
		return home, false
	}
	return filepath.Join(home, p[2:]), false
}

// splitOmpDelimitedPaths mirrors OMP's splitDelimitedPathEntry: existing literal paths stay
// intact (so filenames with spaces/commas/semicolons stay whole); otherwise entries split on
// top-level ';' or ',' outside '{...}' braces, and on top-level whitespace when any part
// (with or without a line selector) resolves on disk.
func splitOmpDelimitedPaths(entry, cwd string) []string {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return nil
	}
	if statReachExists(entry, cwd) {
		return []string{entry}
	}
	if parts := splitTopLevel(entry, false); len(parts) >= 2 {
		return parts
	}
	if parts := splitTopLevel(entry, true); len(parts) >= 2 {
		for _, p := range parts {
			if statReachExists(p, cwd) {
				return parts
			}
		}
	}
	return []string{entry}
}

func splitTopLevel(s string, whitespace bool) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '\\' && runtime.GOOS != "windows" && i+1 < len(s) {
			i++
			continue
		}
		if ch == '{' {
			depth++
			continue
		}
		if ch == '}' && depth > 0 {
			depth--
			continue
		}
		if depth != 0 {
			continue
		}
		sep := ch == ';' || ch == ',' || (whitespace && (ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r'))
		if sep {
			if seg := strings.TrimSpace(s[start:i]); seg != "" {
				parts = append(parts, seg)
			}
			start = i + 1
		}
	}
	if seg := strings.TrimSpace(s[start:]); seg != "" {
		parts = append(parts, seg)
	}
	return parts
}

func statReachExists(raw, cwd string) bool {
	p, unknown := resolveReachPath(raw)
	if unknown || p == "" {
		return false
	}
	check := func(cand string) bool {
		if !isAbsToolPath(cand) && cwd != "" {
			cand = filepath.Join(cwd, cand)
		}
		_, err := os.Stat(cand)
		return err == nil
	}
	if check(p) {
		return true
	}
	if stripped := stripPathSelector(p); stripped != p {
		return check(stripped)
	}
	return false
}

// peelSelectorIfMissing peels a trailing line selector when the raw path does not exist on
// disk so reach classification sees the actual file path rather than path:start-end.
func peelSelectorIfMissing(resolved, cwd string) string {
	stripped := stripPathSelector(resolved)
	if stripped == resolved {
		return resolved
	}
	cand := resolved
	if !isAbsToolPath(cand) && cwd != "" {
		cand = filepath.Join(cwd, cand)
	}
	if _, err := os.Stat(cand); err == nil {
		return resolved
	}
	return stripped
}

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
		if strings.HasSuffix(ent.Name(), ".jsonl") {
			return true
		}
	}
	return false
}
