package procinfo

// Signature is who signed a running process, as the kernel knows it.
type Signature struct {
	// Start is when the process started, in microseconds since the Unix epoch: with the
	// pid, which process this is.
	Start int64
	// Valid is whether its signature still holds; Platform whether Apple signed it.
	Valid, Platform bool
	// Identifier is its signing identifier (ai.terma.desktop), TeamID its developer's team.
	Identifier, TeamID string
}
