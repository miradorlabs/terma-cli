package antigravity

import (
	"fmt"
	"os"
	"testing"
)

// TestMain gives the package a private HOME and no T3CODE_HOME, so no test reads or writes
// the developer's Antigravity homes, ~/.gemini or T3 Code's; a test that needs a home sets
// its own.
func TestMain(m *testing.M) {
	os.Exit(runIsolated(m))
}

func runIsolated(m *testing.M) int {
	home, err := os.MkdirTemp("", "terma-antigravity-home")
	if err != nil {
		fmt.Fprintln(os.Stderr, "private home:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(home) }()
	for _, k := range []string{"HOME", "USERPROFILE"} {
		_ = os.Setenv(k, home)
	}
	_ = os.Unsetenv("T3CODE_HOME")
	return m.Run()
}
