// Package serverkey knows the shape of a Terma project server key, so every place
// that has to recognise one — a harness config, the keystore, a --api-key flag —
// agrees on what counts.
package serverkey

import (
	"regexp"
	"strings"
)

// Prefixes a server key may carry, newest first. Keys are a prefix plus hex. The
// backend minted mir_srv_ keys before Terma had a prefix of its own; those keys
// stay valid, so the CLI must keep accepting them alongside ter_srv_.
var Prefixes = []string{"ter_srv_", "mir_srv_"}

// Pattern matches a server key anywhere in text, such as inside a helper script.
// The character class is exact: nothing else in a config file looks like this.
var Pattern = regexp.MustCompile(`(?:ter|mir)_srv_[0-9a-f]+`)

// Is reports whether s carries a server-key prefix.
func Is(s string) bool {
	for _, p := range Prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Display is the prefix to name in help and error text.
const Display = "ter_srv_"

// Mask renders a key as a recognisable but unusable head: the prefix plus a few
// characters — enough to match against a key list in the web app, far short of
// enough to authenticate. Anything too short to have a head becomes "…".
func Mask(key string) string {
	const keep = 12
	if len(key) <= keep {
		return "…"
	}
	return key[:keep] + "…"
}
