// Package serverkey knows the shape of a Terma project server key, so every place
// that has to recognise one — a harness config, the keystore, a --api-key flag —
// agrees on what counts.
package serverkey

import (
	"regexp"
	"strings"
)

// Pattern matches a server key anywhere in text, such as inside a helper script.
// Include Mirador keys when inspecting existing settings so they stay masked.
// Is separately restricts keys accepted for new Terma connections.
var Pattern = regexp.MustCompile(`(?:ter|mir)_srv_[0-9a-f]+`)

// Is reports whether s carries a server-key prefix.
func Is(s string) bool { return strings.HasPrefix(s, Display) }

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
