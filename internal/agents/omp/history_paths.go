package omp

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var (
	ompInternalURLRe      = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)
	ompStrayColonTargetRe = regexp.MustCompile(`^(?:[/\\~]|\.\.?[/\\]|[A-Za-z]:)`)
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

func decodeOmpPath(raw string) (string, bool) {
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

func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

func isWindowsDrivePath(p string) bool {
	return len(p) >= 2 && isASCIILetter(p[0]) && p[1] == ':' && (len(p) == 2 || p[2] == '\\' || p[2] == '/')
}

func isAbsToolPath(p string) bool {
	return filepath.IsAbs(p) || (len(p) >= 3 && isWindowsDrivePath(p))
}

func translateWindowsDriveOnLinux(p string) (string, bool) {
	if strings.HasPrefix(p, `\\`) {
		return p, true
	}
	if !isWindowsDrivePath(p) {
		return p, false
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != "" {
		drive := strings.ToLower(p[:1])
		mount := "/mnt/" + drive
		if fi, err := os.Stat(mount); err == nil && fi.IsDir() {
			rest := strings.TrimLeft(strings.ReplaceAll(p[2:], `\`, "/"), "/")
			if rest == "" {
				return mount, false
			}
			return mount + "/" + rest, false
		}
	}
	return p, true
}

// resolveOmpPath normalizes OMP path shorthands, decodes local file:// URLs, filters
// internal URLs, translates or fails closed on Windows drive/UNC paths under Linux/WSL, and
// expands a leading ~ / ~/ / ~\ to the user's home directory so an edited file's path never
// joins ~/... under the session cwd. The bool reports a path it cannot place on this machine.
func resolveOmpPath(raw string) (string, bool) {
	p, unknown := decodeOmpPath(raw)
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
