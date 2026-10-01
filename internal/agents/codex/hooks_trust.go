package codex

import (
	"path/filepath"
	"strings"
)

// Codex skips a hook until the developer trusts that exact entry's hash in Codex, recorded
// under `[hooks.state]` in the user config as `<hooks.json path>:<event>:<group>:<hook>`,
// the event in snake_case. Terma never writes this table: trusting is the developer's act.
const (
	codexHooksStateTable = "hooks"
	codexHooksStateKey   = "state"
	codexTrustedHashKey  = "trusted_hash"
	codexHookEnabledKey  = "enabled"
)

// hookTrust is what the user's Codex config says about one hooks file.
type hookTrust struct {
	ConfigPath string
	// Entries is how many hooks from this file Codex has a record for.
	Entries int
	// Trusted is how many of those carry a trusted hash.
	Trusted  int
	Disabled int
	// TrustedKeys names the trusted entries ("<event>:<group>:<handler>"): an entry added to
	// an already-trusted file has no record and is skipped without a word.
	TrustedKeys map[string]bool
	// TrustedHashes are Codex's recorded hashes by entry key; a changed entry no longer matches.
	TrustedHashes map[string]string
}

// Reviewed reports whether Codex has any record for this file; a fresh clone has none.
func (t hookTrust) Reviewed() bool { return t.Entries > 0 }

// hookTrustFor reports what the user's Codex config records about the hooks file at
// hooksPath; an unreadable or absent config means nothing is trusted.
func (c exporter) hookTrustFor(hooksPath string) (hookTrust, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return hookTrust{}, err
	}
	trust := hookTrust{ConfigPath: path}
	f, err := loadTOML(path)
	if err != nil {
		return trust, err
	}
	hooks, _ := f.doc[codexHooksStateTable].(map[string]any)
	state, _ := hooks[codexHooksStateKey].(map[string]any)
	if len(state) == 0 {
		return trust, nil
	}
	// Codex records the path it loaded: compare resolved, then literal, so a symlinked
	// repository does not read as never trusted.
	prefixes := []string{hooksPath + ":"}
	if resolved, err := filepath.EvalSymlinks(hooksPath); err == nil && resolved != hooksPath {
		prefixes = append(prefixes, resolved+":")
	}
	for key, raw := range state {
		matched, name := false, ""
		for _, prefix := range prefixes {
			if after, ok := strings.CutPrefix(key, prefix); ok {
				matched, name = true, after
				break
			}
		}
		if !matched {
			continue
		}
		trust.Entries++
		entry, _ := raw.(map[string]any)
		if hash, ok := entry[codexTrustedHashKey].(string); ok && strings.TrimSpace(hash) != "" {
			trust.Trusted++
			if trust.TrustedKeys == nil {
				trust.TrustedKeys = map[string]bool{}
				trust.TrustedHashes = map[string]string{}
			}
			trust.TrustedKeys[name] = true
			trust.TrustedHashes[name] = hash
		}
		if enabled, ok := entry[codexHookEnabledKey].(bool); ok && !enabled {
			trust.Disabled++
		}
	}
	return trust, nil
}
