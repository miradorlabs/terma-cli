package procinfo

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
	t.Parallel()
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
	port, server := conn.RemoteAddr().(*net.TCPAddr).Port, conn.LocalAddr().(*net.TCPAddr).Port
	if pid, ok := FindSender(port, server); !ok || pid != child.Process.Pid {
		t.Fatalf("FindSender(%d, %d) = %d, %v; want the child %d", port, server, pid, ok, child.Process.Pid)
	}
}

// connectEnv makes the test binary the connecting child: it dials the address and
// holds the connection until its stdin closes. bindEnv makes it a process holding a socket
// on [::1] at the given port, says "bound" or "no" on stdout, and holds it until stdin
// closes.
const (
	connectEnv = "PROCINFO_TEST_CONNECT"
	bindEnv    = "PROCINFO_TEST_BIND"
)

// A newer process holding the sender's port number on another address is not the sender:
// matched on the port alone, it was, and a session's telemetry went to it (macOS lists the
// newest process first).
func TestFindSenderMatchesBothEnds(t *testing.T) {
	t.Parallel()
	if !Supported {
		t.Skip("unsupported")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	start := func(env string) (*exec.Cmd, io.ReadCloser) {
		child := exec.Command(os.Args[0], "-test.run=^$")
		child.Env = append(os.Environ(), env)
		stdin, _ := child.StdinPipe()
		stdout, _ := child.StdoutPipe()
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stdin.Close(); _ = child.Process.Kill(); _ = child.Wait() })
		return child, stdout
	}
	sender, _ := start(connectEnv + "=" + ln.Addr().String())
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	port, server := conn.RemoteAddr().(*net.TCPAddr).Port, conn.LocalAddr().(*net.TCPAddr).Port
	_, out := start(fmt.Sprint(bindEnv, "=", port))
	said := make([]byte, 5)
	if n, _ := io.ReadFull(out, said); string(said[:n]) != "bound" {
		t.Skipf("no [::1]:%d here to share the port number", port)
	}
	if pid, ok := FindSender(port, server); !ok || pid != sender.Process.Pid {
		t.Fatalf("FindSender(%d, %d) = %d, %v; want the sender %d, not the process holding [::1]:%d", port, server, pid, ok, sender.Process.Pid, port)
	}
}

// executableEnv makes the test binary print AbsExecutable and exit.
const executableEnv = "PROCINFO_TEST_EXECUTABLE"

// A terma started through a symlink, as Homebrew links one, is that symlink: the link names
// the new build after an upgrade, where its target (Linux's os.Executable) is removed.
// Started by a bare name, it is the PATH entry the name found.
func TestAbsExecutableIsThePathStartedAs(t *testing.T) {
	bin := t.TempDir()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe" // Windows starts only a file with an executable extension
	}
	link := filepath.Join(bin, "terma-link"+ext)
	if err := os.Symlink(os.Args[0], link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	for _, tc := range []struct{ name, run string }{{"by path", link}, {"by name on PATH", "terma-link"}} {
		t.Run(tc.name, func(t *testing.T) {
			child := exec.Command(link, "-test.run=^$")
			child.Args[0] = tc.run
			child.Env = append(os.Environ(), executableEnv+"=1", "PATH="+bin)
			out, err := child.Output()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(out)); got != link {
				t.Fatalf("AbsExecutable = %q, want the symlink %q", got, link)
			}
		})
	}
}

func TestMain(m *testing.M) {
	if os.Getenv(executableEnv) != "" {
		exe, err := AbsExecutable()
		if err != nil {
			os.Exit(2)
		}
		fmt.Println(exe)
		os.Exit(0)
	}
	if port := os.Getenv(bindEnv); port != "" {
		ln, err := net.Listen("tcp6", "[::1]:"+port)
		if err != nil {
			fmt.Print("no")
			os.Exit(0)
		}
		fmt.Print("bound")
		_, _ = io.Copy(io.Discard, os.Stdin)
		_ = ln.Close()
		os.Exit(0)
	}
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
		FindSender(1, 2)
	}
}
