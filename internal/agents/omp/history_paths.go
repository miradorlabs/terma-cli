package omp

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

var (
	ompInternalURLRe      = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)
	ompStrayColonTargetRe = regexp.MustCompile(`^(?:[/\\~]|\.\.?[/\\]|[A-Za-z]:)`)
	ompLineRangeRe        = regexp.MustCompile(`(?i)(?::(?:(?:\d+(?:(?:-|\+)\d+)?|-\d+)(?:,(?:\d+(?:(?:-|\+)\d+)?|-\d+))*|raw|conflicts|img)|#L?\d+(?:-L?\d+)?)$`)
)

func stripOuterDoubleQuotes(p string) string {
	if len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"' {
		return strings.TrimSpace(p[1 : len(p)-1])
	}
	return p
}

func normalizeUnicodeSpaces(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\u00A0' || (r >= '\u2000' && r <= '\u200A') || r == '\u202F' || r == '\u205F' || r == '\u3000' {
			return ' '
		}
		return r
	}, s)
}

func stripWindowsExtendedPrefix(p string) string {
	switch {
	case len(p) >= 8 && strings.EqualFold(p[:8], `\\?\UNC\`):
		return `\\` + p[8:]
	case len(p) >= 8 && strings.EqualFold(p[:8], `//?/UNC/`):
		return `//` + p[8:]
	case strings.HasPrefix(p, `\\?\`) || strings.HasPrefix(p, `//?/`):
		return p[4:]
	}
	return p
}

// normalizeOmpPath mirrors OMP's expandPath / normalizeAtPrefix / stripOuterDoubleQuotes
// before URL decoding and tilde expansion: Unicode spaces, outer double quotes, stray leading
// ':' before a path root, leading '@' shorthands (@/..., @~/..., @../..., @./..., @C:\...,
// @\\..., @scheme://...), and Windows extended-length prefixes (\\?\).
func normalizeOmpPath(raw string) string {
	p := stripOuterDoubleQuotes(strings.TrimSpace(normalizeUnicodeSpaces(raw)))
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, ":") {
		if rest := stripOuterDoubleQuotes(p[1:]); ompStrayColonTargetRe.MatchString(rest) {
			p = rest
		}
	}
	if strings.HasPrefix(p, "@") {
		rest := stripOuterDoubleQuotes(p[1:])
		if strings.HasPrefix(rest, "/") || strings.HasPrefix(rest, `\`) ||
			rest == "~" || strings.HasPrefix(rest, "~/") || strings.HasPrefix(rest, `~\`) ||
			rest == ".." || strings.HasPrefix(rest, "../") || strings.HasPrefix(rest, `..\`) ||
			strings.HasPrefix(rest, "./") || strings.HasPrefix(rest, `.\`) ||
			isAbsToolPath(rest) || ompInternalURLRe.MatchString(rest) {
			p = rest
		}
	}
	return stripWindowsExtendedPrefix(p)
}

func decodeReachPath(raw string) (string, bool) {
	p := normalizeOmpPath(raw)
	if p == "" {
		return "", false
	}
	if len(p) >= 7 && strings.EqualFold(p[:7], "file://") {
		u, err := url.Parse(p)
		if err != nil || u.Path == "" {
			return p, true
		}
		if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
			return p, true
		}
		decoded := u.Path
		if len(decoded) >= 3 && decoded[0] == '/' && isASCIILetter(decoded[1]) && decoded[2] == ':' {
			decoded = decoded[1:]
		}
		return stripWindowsExtendedPrefix(decoded), false
	}
	if idx := strings.Index(p, "://"); idx > 0 && ompInternalURLRe.MatchString(p) {
		switch strings.ToLower(p[:idx]) {
		case "agent", "attachment", "cfg", "conflict", "help", "local", "memory", "omp", "out", "rule", "security", "skill", "http", "https":
			return "", false
		}
		return p, true
	}
	return p, false
}

// nonURLPath normalizes OMP path shorthands, converts local file:// URLs to filesystem paths,
// and filters out non-filesystem OMP internal URLs (agent://, cfg://, local://, http://, etc.).
func nonURLPath(p string) string {
	decoded, unknown := decodeReachPath(p)
	if unknown {
		return ""
	}
	return decoded
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

func isWindowsDrivePath(p string) bool {
	return len(p) >= 2 && isASCIILetter(p[0]) && p[1] == ':' && (len(p) == 2 || p[2] == '\\' || p[2] == '/')
}

func isAbsToolPath(p string) bool {
	return filepath.IsAbs(p) || (len(p) >= 3 && isWindowsDrivePath(p))
}

// resolveReachPath normalizes OMP path shorthands, decodes local file:// URLs, filters
// internal URLs, translates or fails closed on Windows drive/UNC paths under Linux/WSL, and
// expands a leading ~ / ~/ / ~\ to the user's home directory so reach classification never
// joins ~/... under the session cwd.
func resolveReachPath(raw string) (string, bool) {
	p, unknown := decodeReachPath(raw)
	if unknown || p == "" {
		return p, unknown
	}
	if runtime.GOOS != "windows" {
		if translated, unk := translateWindowsDriveOnLinux(p); unk || translated != p {
			return translated, unk
		}
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

// splitOmpDelimitedPaths mirrors OMP's splitDelimitedPathEntry while remaining fail-closed
// after referenced files have been deleted: an existing literal path stays intact, otherwise
// entries split on top-level ';' or ',' outside '{...}' braces. When a segment does not exist
// on disk and has top-level whitespace, live whitespace parts suppress the unsplit segment;
// when none are live, a deleted path inside an existing spaced enclosing directory stays whole
// unless a trailing token looks like an independent path root, otherwise both forms are kept.
func splitOmpDelimitedPaths(entry, cwd string) []string {
	entry = strings.TrimSpace(normalizeUnicodeSpaces(entry))
	if entry == "" {
		return nil
	}
	if statReachExists(entry, cwd) {
		return []string{entry}
	}
	primary := splitTopLevel(entry, false, true)
	if len(primary) == 0 {
		return nil
	}
	var out []string
	add := func(s string) {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, seg := range primary {
		if statReachExists(seg, cwd) {
			add(seg)
			continue
		}
		if ws := splitTopLevel(seg, true, true); len(ws) >= 2 {
			anyLive := false
			for _, w := range ws {
				if statReachExists(w, cwd) {
					anyLive = true
					break
				}
			}
			if !anyLive && spacedEnclosingDirExists(seg, ws, cwd) {
				add(seg)
				continue
			}
			if !anyLive {
				add(seg)
			}
			for _, w := range ws {
				add(w)
			}
			continue
		}
		add(seg)
	}
	return out
}

func spacedEnclosingDirExists(seg string, ws []string, cwd string) bool {
	for _, tok := range ws[1:] {
		p := normalizeOmpPath(tok)
		if p == "~" || p == ".." || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) ||
			strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) ||
			strings.HasPrefix(p, "../") || strings.HasPrefix(p, `..\`) ||
			slices.Contains(strings.Split(strings.ReplaceAll(p, `\`, "/"), "/"), "..") ||
			isWindowsDrivePath(p) || ompInternalURLRe.MatchString(p) {
			return false
		}
	}
	joinCwd := func(raw string) (string, bool) {
		p, unk := resolveReachPath(raw)
		if unk || p == "" {
			return "", false
		}
		p = stripPathSelector(p)
		if !isAbsToolPath(p) && cwd != "" {
			p = filepath.Join(cwd, p)
		}
		return filepath.Clean(p), true
	}
	cand, ok1 := joinCwd(seg)
	firstTok, ok2 := joinCwd(ws[0])
	if !ok1 || !ok2 {
		return false
	}
	for dir := filepath.Dir(cand); len(dir) > len(firstTok); {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return strings.HasPrefix(dir, firstTok) && (dir[len(firstTok)] == ' ' || dir[len(firstTok)] == '\t')
		}
		if parent := filepath.Dir(dir); parent != dir {
			dir = parent
		} else {
			break
		}
	}
	return false
}

func splitTopLevel(s string, whitespace, comma bool) []string {
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
		sep := ch == ';' || (comma && ch == ',') || (whitespace && (ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r'))
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
