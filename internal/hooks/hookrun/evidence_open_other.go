//go:build !unix

package hookrun

// EvidenceOpenFlags add nothing where the platform has no O_NOFOLLOW.
const EvidenceOpenFlags = 0
