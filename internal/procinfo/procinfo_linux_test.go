package procinfo

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// A process whose file was removed since it started still names that file's path, and
// reaches the file it runs through /proc. Not parallel: a child another test forks while the
// copy is open for writing would keep it busy (ETXTBSY) when this one starts it.
func TestExecutableOfARemovedFile(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep here")
	}
	copied := filepath.Join(t.TempDir(), "agy")
	copyFile(t, sleep, copied)
	child := exec.Command(copied, "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	if err := os.Remove(copied); err != nil {
		t.Fatal(err)
	}
	got, ok := ExecutableOf(child.Process.Pid)
	if !ok {
		t.Fatal("ExecutableOf could not tell a removed file's process")
	}
	want := Executable{Path: copied, Run: filepath.Join("/proc", strconv.Itoa(child.Process.Pid), "exe")}
	if got != want {
		t.Fatalf("ExecutableOf = %+v, want %+v", got, want)
	}
	if _, err := os.Stat(got.Run); err != nil {
		t.Fatalf("the running file cannot be reached: %v", err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
