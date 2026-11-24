package procinfo

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// ExecutableOf names the file this test binary runs, where the system can tell.
func TestExecutableOfNamesThisProcess(t *testing.T) {
	t.Parallel()
	got, ok := ExecutableOf(os.Getpid())
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		if ok {
			t.Fatalf("ExecutableOf answered %+v where it cannot tell", got)
		}
		return
	}
	if !ok {
		t.Fatal("ExecutableOf could not tell this process's file")
	}
	want, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got.Path) != filepath.Base(want) {
		t.Fatalf("ExecutableOf path %q, want the test binary %q", got.Path, want)
	}
	run, err := os.Stat(got.Run)
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(run, self) {
		t.Fatalf("ExecutableOf runs %q, not the test binary %q", got.Run, want)
	}
}

func TestExecutableOfAProcessThatDoesNotExist(t *testing.T) {
	t.Parallel()
	for _, pid := range []int{0, -1, 1 << 30} {
		if got, ok := ExecutableOf(pid); ok {
			t.Errorf("ExecutableOf(%d) = %+v", pid, got)
		}
	}
}
