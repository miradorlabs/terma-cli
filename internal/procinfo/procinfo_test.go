package procinfo

import (
	"net"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestAncestorsStartAtTheParent(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("unsupported")
	}
	got := Ancestors()
	if len(got) == 0 || got[0] != os.Getppid() {
		t.Fatalf("ancestors %v do not start at the parent %d", got, os.Getppid())
	}
	if slices.Contains(got, 1) || slices.Contains(got, os.Getpid()) {
		t.Fatalf("ancestors %v hold pid 1 or this process", got)
	}
}

// A child process connects to a listener here; FindSender names the child — not this
// process, which holds the server end of the same connection. This is also what pins
// the proc_info layout on macOS against a real socket.
func TestFindSenderNamesTheConnectingProcess(t *testing.T) {
	if !Supported {
		t.Skip("unsupported")
	}
	if _, err := exec.LookPath("/usr/bin/nc"); err != nil {
		t.Skip("nc not installed")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	child := exec.Command("/usr/bin/nc", "127.0.0.1", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	stdin, _ := child.StdinPipe()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = child.Process.Kill(); _ = child.Wait() }()
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	remote := conn.RemoteAddr().String()
	port, _ := strconv.Atoi(remote[strings.LastIndexByte(remote, ':')+1:])
	if pid, ok := FindSender(port); !ok || pid != child.Process.Pid {
		t.Fatalf("FindSender(%d) = %d, %v; want the child %d", port, pid, ok, child.Process.Pid)
	}
}

func BenchmarkFindSender(b *testing.B) {
	for b.Loop() {
		FindSender(1)
	}
}
