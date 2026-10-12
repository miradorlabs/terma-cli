package procinfo

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	t.Parallel()
	if !Supported {
		t.Skip("unsupported")
	}
	// Over IPv4, as the relay listens, and IPv6, whose addresses each platform lays out apart;
	// and to a listener on every address, which sees an IPv4 client as IPv4-mapped.
	for _, network := range []struct{ name, listen, dial string }{
		{"tcp4", "127.0.0.1:0", ""}, {"tcp6", "[::1]:0", ""}, {"tcp", "[::]:0", "127.0.0.1"},
	} {
		t.Run(network.name+" "+network.listen, func(t *testing.T) {
			t.Parallel()
			ln, err := net.Listen(network.name, network.listen)
			if err != nil {
				if network.name != "tcp4" {
					t.Skipf("no %s here: %v", network.listen, err)
				}
				t.Fatal(err)
			}
			defer func() { _ = ln.Close() }()
			addr := ln.Addr().String()
			if network.dial != "" {
				addr = net.JoinHostPort(network.dial, strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
			}
			child := exec.Command(os.Args[0], "-test.run=^$")
			child.Env = append(os.Environ(), connectEnv+"="+addr)
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
			client, server := conn.RemoteAddr().(*net.TCPAddr).AddrPort(), conn.LocalAddr().(*net.TCPAddr).AddrPort()
			if pid, ok := FindSender(client, server); !ok || pid != child.Process.Pid {
				t.Fatalf("FindSender(%v, %v) = %d, %v; want the child %d", client, server, pid, ok, child.Process.Pid)
			}
		})
	}
}

// connectEnv makes the test binary the connecting child: it dials the address and
// holds the connection until its stdin closes. decoyEnv, "FROM TO", makes it dial TO from
// FROM, say "bound" or "no" on stdout, and hold that until stdin closes.
const (
	connectEnv = "PROCINFO_TEST_CONNECT"
	decoyEnv   = "PROCINFO_TEST_DECOY"
)

// A newer process holding a connection with the sender's two port numbers, between other
// addresses, is not the sender: matched on ports, it was, and a session's telemetry went to it
// (macOS lists the newest process first).
func TestFindSenderMatchesTheWholeConnection(t *testing.T) {
	t.Parallel()
	if !Supported {
		t.Skip("unsupported")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	ln6, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
	if err != nil {
		t.Skipf("no [::1]:%d here for a connection with the same ports: %v", port, err)
	}
	defer func() { _ = ln6.Close() }()
	start := func(env string) (*exec.Cmd, io.ReadCloser) {
		child := exec.Command(os.Args[0], "-test.run=^$")
		child.Env = append(os.Environ(), env)
		stdin, err := child.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := child.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stdin.Close(); _ = child.Process.Kill(); _ = child.Wait() })
		return child, stdout
	}
	sender, _ := start(connectEnv + "=" + ln.Addr().String())
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	client, server := c.RemoteAddr().(*net.TCPAddr).AddrPort(), c.LocalAddr().(*net.TCPAddr).AddrPort()
	decoy, out := start(fmt.Sprintf("%s=[::1]:%d [::1]:%d", decoyEnv, client.Port(), server.Port()))
	said, _ := io.ReadAll(io.LimitReader(out, 5))
	switch string(said) {
	case "bound":
	case "no":
		t.Skipf("no [::1]:%d here to connect from", client.Port())
	default:
		t.Fatalf("the decoy said %q", said)
	}
	if dc, err := ln6.Accept(); err == nil {
		defer func() { _ = dc.Close() }()
	}
	if pid, ok := FindSender(client, server); !ok || pid != sender.Process.Pid {
		t.Fatalf("FindSender(%v, %v) = %d, %v; want the sender %d, not the decoy %d on [::1]", client, server, pid, ok, sender.Process.Pid, decoy.Process.Pid)
	}
	// Where the platform asks each process, the decoy is refused outright: where it lists the
	// sender first (Linux, oldest first), FindSender alone would pass on the ports.
	whole := conn{client: client, server: server}
	if ownsConnForTest != nil && (ownsConnForTest(decoy.Process.Pid, whole) || !ownsConnForTest(sender.Process.Pid, whole)) {
		t.Fatalf("the decoy %d is taken for %v→%v, or the sender %d is not", decoy.Process.Pid, client, server, sender.Process.Pid)
	}
}

// ownsConnForTest is ownsConn, where the platform asks each process (procinfo_scan_test.go).
var ownsConnForTest func(pid int, c conn) bool

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
	if ends := os.Getenv(decoyEnv); ends != "" {
		from, to, _ := strings.Cut(ends, " ")
		local, err := net.ResolveTCPAddr("tcp6", from)
		var c net.Conn
		if err == nil {
			c, err = (&net.Dialer{LocalAddr: local}).Dial("tcp6", to)
		}
		if err != nil {
			fmt.Print("no")
			os.Exit(0)
		}
		fmt.Print("bound")
		_, _ = io.Copy(io.Discard, os.Stdin)
		_ = c.Close()
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
		FindSender(netip.MustParseAddrPort("127.0.0.1:1"), netip.MustParseAddrPort("127.0.0.1:2"))
	}
}
