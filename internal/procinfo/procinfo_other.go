//go:build !darwin && !linux

package procinfo

func parentOf(int) (int, bool) { return 0, false }

// Supported is false: the relay then matches claims by session alone.
const Supported = false

func ownsPort(int, int) bool { return false }

func allPIDs() []int { return nil }
