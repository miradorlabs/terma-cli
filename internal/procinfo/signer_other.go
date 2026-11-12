//go:build !darwin

package procinfo

// Signer is who signed pid: only macOS's kernel says.
func Signer(int) (Signature, bool) { return Signature{}, false }
