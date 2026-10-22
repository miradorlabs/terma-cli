//go:build !unix

package procinfo

// WriteDenied reports no refusal where terma does not ask.
func WriteDenied(string) bool { return false }
