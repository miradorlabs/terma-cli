//go:build !darwin && !linux && !windows

package procinfo

func parentOf(int) (int, bool) { return 0, false }

// Supported is false: the relay then matches claims by session alone.
const Supported = false

func findSender(int, int) (int, bool) { return 0, false }
