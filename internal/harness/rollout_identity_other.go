//go:build !unix

package harness

import "os"

// Head and checkpoint hashes detect content changes on other platforms. A
// byte-identical replacement is intentionally indistinguishable from the original.
func rolloutFileIdentity(st os.FileInfo) string { return "" }
