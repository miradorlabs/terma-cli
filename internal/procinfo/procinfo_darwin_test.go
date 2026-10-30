package procinfo

import (
	"slices"
	"testing"
)

// KERN_PROCARGS2's layout: the environment follows the padded executable path and argc
// arguments, an empty argument included.
func TestProcArgsEnvSkipsThePathAndArguments(t *testing.T) {
	t.Parallel()
	buf := []byte("\x03\x00\x00\x00/usr/local/bin/agent\x00\x00\x00\x00agent\x00\x00--flag\x00A=1\x00B=two=2\x00\x00junk\x00")
	got, ok := procArgsEnv(buf)
	if want := []string{"A=1", "B=two=2"}; !ok || !slices.Equal(got, want) {
		t.Fatalf("procArgsEnv = %q, %v; want %q", got, ok, want)
	}
	if _, ok := procArgsEnv([]byte("\x01\x00")); ok {
		t.Fatal("a truncated buffer was read")
	}
}
