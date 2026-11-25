package codex

import (
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// codexNormalizeCwd turns a recorded working directory (possibly a file:// URL) into a
// path. A relative result is taken against fallback when there is one, since
// CommandExecution and FileChange items can record a cwd relative to the session's.
func codexNormalizeCwd(raw, fallback string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fallback
	}
	if strings.HasPrefix(s, "file://") {
		s = fileURLPath(s, runtime.GOOS == "windows")
	}
	if !filepath.IsAbs(s) && fallback != "" {
		return filepath.Join(fallback, s)
	}
	return s
}

// fileURLPath returns the path a file:// URL names, as the operating system writes it.
// On Unix that is the URL's path, host ignored. On Windows (windows true) the slashes
// turn to backslashes, a drive URL (file:///C:/x) loses the slash before its drive
// letter, and a host other than localhost makes a UNC path (\\host\share\x). It never
// reads runtime.GOOS itself, so both forms can be tested on any machine.
func fileURLPath(raw string, windows bool) string {
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return strings.TrimPrefix(raw, "file://")
	}
	if !windows {
		return u.Path
	}
	p := strings.ReplaceAll(u.Path, "/", `\`)
	if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
		return `\\` + u.Host + p
	}
	if len(p) >= 3 && p[0] == '\\' && isASCIILetter(p[1]) && p[2] == ':' {
		return p[1:]
	}
	return p
}

func isASCIILetter(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}
