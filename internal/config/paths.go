package config

import (
	"path"
	"strings"
)

// ExcludesPath matches repository-relative globs, including ** across directories.
// A basename pattern (.env) also matches that basename in a nested directory.
// Ancestors are checked so excluding a directory excludes everything under it.
func (p Policy) ExcludesPath(name, root string) bool {
	name = strings.ReplaceAll(name, "\\", "/")
	root = strings.TrimRight(strings.ReplaceAll(root, "\\", "/"), "/")
	if root != "" {
		name = strings.TrimPrefix(name, root+"/")
	}
	name = path.Clean(name)
	for _, pattern := range p.ExcludePaths {
		pattern = strings.ReplaceAll(pattern, "\\", "/")
		if pattern != "/" {
			pattern = strings.TrimRight(pattern, "/")
		}
		for n := name; n != "." && n != "/" && n != ""; n = path.Dir(n) {
			candidate := n
			if !strings.Contains(pattern, "/") {
				candidate = path.Base(n)
			}
			patternParts, candidateParts := strings.Split(pattern, "/"), strings.Split(candidate, "/")
			if globPath(patternParts, candidateParts) {
				return true
			}
			// Native telemetry often names an absolute file without its workspace
			// root. Check each possible relative suffix rather than authorize it
			// merely because the exporter omitted that context.
			absolute := strings.HasPrefix(candidate, "/") || len(candidate) > 2 && candidate[1] == ':' && candidate[2] == '/'
			if absolute && !strings.HasPrefix(pattern, "/") {
				for i := 1; i < len(candidateParts); i++ {
					if globPath(patternParts, candidateParts[i:]) {
						return true
					}
				}
			}
		}
	}
	return false
}

func globPath(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if globPath(pattern[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, err := path.Match(pattern[0], name[0])
	return err != nil || ok && globPath(pattern[1:], name[1:])
}
