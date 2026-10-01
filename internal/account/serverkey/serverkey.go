// Package serverkey knows the shape of a Terma project server key.
package serverkey

import (
	"regexp"
	"strings"
)

// Pattern matches a server key anywhere in text, Mirador's included so they stay masked.
var Pattern = regexp.MustCompile(`(?:ter|mir)_srv_[0-9a-f]+`)

// Is reports whether s carries a server-key prefix.
func Is(s string) bool { return strings.HasPrefix(s, Display) }

// Display is the prefix to name in help and error text.
const Display = "ter_srv_"

// Mask renders a key as a recognisable but unusable head.
func Mask(key string) string {
	const keep = 12
	if len(key) <= keep {
		return "…"
	}
	return key[:keep] + "…"
}
