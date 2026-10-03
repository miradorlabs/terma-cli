// Package project names working copies: their safe ids and where their local session
// state lives.
package project

import (
	"regexp"
	"strings"
)

// safeID matches session.ValidID's charset: both ids become file names.
var safeID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// maxIDLen bounds an id long before any filesystem does.
const maxIDLen = 128

// ValidID reports whether id is safe as a path component: an id like "../../.zshenv"
// would otherwise place a key-holding file there.
func ValidID(id string) bool {
	return id != "" && len(id) <= maxIDLen && safeID.MatchString(id) && !strings.HasPrefix(id, ".")
}
