//go:build !unix

package codex

import "os"

// Off Unix, content hashes detect a change; a byte-identical replacement is not one.
func rolloutFileIdentity(st os.FileInfo) string { return "" }
