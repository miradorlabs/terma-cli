//go:build darwin || linux

package procinfo

import (
	"os"
	"slices"
	"testing"
)

// Environ reads this process's own startup environment, pinning the platform layout, and
// reads nothing for a process that does not exist.
func TestEnvironReadsAProcessStartupEnvironment(t *testing.T) {
	t.Parallel()
	env, ok := Environ(os.Getpid())
	if !ok || !slices.ContainsFunc(os.Environ(), func(kv string) bool { return slices.Contains(env, kv) }) {
		t.Fatalf("Environ(self) = %d entries, %v; want this process's environment", len(env), ok)
	}
	if _, ok := Environ(1 << 30); ok {
		t.Fatal("Environ read a process that does not exist")
	}
}
