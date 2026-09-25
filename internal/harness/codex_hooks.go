package harness

import (
	"path/filepath"
	"strings"
)

// Codex will not run a hook it has not been shown. Every non-managed hook is trusted
// individually, against a hash of that exact entry, and an untrusted one is skipped
// with a warning until the developer reviews it inside Codex. A repository's committed
// hooks therefore do nothing on a fresh clone until that happens once — which is silent
// from terma's side unless something goes looking, and this is what goes looking.
//
// The record lives in the user's config.toml, under `[hooks.state]`, keyed by the hook's
// origin: `<path to hooks.json>:<event>:<group index>:<hook index>`. The event is
// snake_case there (`post_tool_use`), unlike the file itself (`PostToolUse`). Terma
// never writes this table — trusting is the developer's act, made in Codex, and forging
// a hash to skip the prompt would defeat the point of the prompt.
const (
	codexHooksStateTable = "hooks"
	codexHooksStateKey   = "state"
	codexTrustedHashKey  = "trusted_hash"
	codexHookEnabledKey  = "enabled"
)

// CodexHookTrust is what the user's Codex config says about one hooks file.
type CodexHookTrust struct {
	// ConfigPath is the user config the answer was read from.
	ConfigPath string
	// Entries is how many hooks from this file Codex has a record for.
	Entries int
	// Trusted is how many of those carry a trusted hash, so Codex will run them.
	Trusted int
	// Disabled is how many the developer has explicitly switched off.
	Disabled int
	// TrustedKeys is which entries carry a trusted hash, as Codex names them after the
	// file's path: "<event>:<group>:<handler>", the event in snake_case. Counting is not
	// enough — Codex trusts each entry by itself, so an entry a newer terma added to an
	// already-trusted file has no record at all and is skipped without a word.
	TrustedKeys map[string]bool
	// TrustedHashes are Codex's recorded hashes by entry key. A recorded hash
	// does not establish trust when the hook definition has since changed.
	TrustedHashes map[string]string
}

// Reviewed reports whether Codex has been shown this file's hooks at all. A file with
// no record has never been reviewed, which is the ordinary state of a fresh clone.
func (t CodexHookTrust) Reviewed() bool { return t.Entries > 0 }

// CodexHookTrustFor reports what the user's Codex config records about the hooks file
// at hooksPath. An unreadable or absent config is not an error: it means no hook from
// that file has been trusted, which is exactly what a fresh machine looks like.
func (c Codex) CodexHookTrustFor(hooksPath string) (CodexHookTrust, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return CodexHookTrust{}, err
	}
	trust := CodexHookTrust{ConfigPath: path}
	f, err := loadTOML(path)
	if err != nil {
		return trust, err
	}
	hooks, _ := f.doc[codexHooksStateTable].(map[string]any)
	state, _ := hooks[codexHooksStateKey].(map[string]any)
	if len(state) == 0 {
		return trust, nil
	}
	// Codex records the origin as the path it loaded, so compare on the resolved path
	// and fall back to the literal one: a repository reached through a symlink must not
	// read as never trusted.
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
