package procinfo

import (
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestAncestorsStartAtTheParent(t *testing.T) {
	if !Supported {
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

// FindSender names a child connecting here, not this process; it pins the platform
// layouts against a real socket.
func TestFindSenderNamesTheConnectingProcess(t *testing.T) {
	if !Supported {
		t.Skip("unsupported")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), connectEnv+"="+ln.Addr().String())
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

// connectEnv makes the test binary the connecting child: it dials the address and
// holds the connection until its stdin closes.
const connectEnv = "PROCINFO_TEST_CONNECT"

func TestMain(m *testing.M) {
	if addr := os.Getenv(connectEnv); addr != "" {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			os.Exit(2)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		_ = conn.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func BenchmarkFindSender(b *testing.B) {
	for b.Loop() {
		FindSender(1)
	}
}
