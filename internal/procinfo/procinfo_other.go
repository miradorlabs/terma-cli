//go:build !darwin && !linux && !windows

package procinfo

func parentOf(int) (int, bool) { return 0, false }

// Supported is false: the relay then matches claims by session alone.
const Supported = false

func findSender(conn, int) (int, bool) { return 0, false }

func environ(int) ([]string, bool) { return nil, false }
