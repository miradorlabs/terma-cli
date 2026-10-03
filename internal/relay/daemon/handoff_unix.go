//go:build unix

package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const handoffSupported = true

// handoffFile is the socket a stopping relay offers its listener on.
const handoffFile = "handoff.sock"

// maxSocketPath stays under the shortest sun_path (104 bytes on macOS).
const maxSocketPath = 100

// handoffPath is in the relay's private directory, or, where that is too long for a socket
// address, in a private directory of this user's under the system's temporary one.
func handoffPath(dir string) (string, error) {
	if p := filepath.Join(dir, handoffFile); len(p) < maxSocketPath {
		return p, nil
	}
	private := filepath.Join(os.TempDir(), "terma-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(private, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(private)
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !ok || int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("relay: %s is not a private directory", private)
	}
	sum := sha256.Sum256([]byte(filepath.Clean(dir)))
	p := filepath.Join(private, hex.EncodeToString(sum[:8])+".sock")
	if len(p) >= maxSocketPath {
		return "", errors.New("relay: no short private path for the handoff socket")
	}
	return p, nil
}

// listenerFile is a duplicate of ln's socket, which outlives ln's Close.
func listenerFile(ln net.Listener) (*os.File, error) {
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		return nil, errors.New("relay: not a TCP listener")
	}
	return tl.File()
}

// offerListener listens for the successor; the returned func sends it sock with addr, the
// address it listens on, and reports whether the successor took it.
func offerListener(dir, addr string, sock *os.File) (func(time.Duration) bool, error) {
	path, err := handoffPath(dir)
	if err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return func(wait time.Duration) bool {
		defer func() { _ = l.Close() }()
		_ = l.SetDeadline(time.Now().Add(wait))
		c, err := l.AcceptUnix()
		if err != nil {
			return false
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		raw, err := sock.SyscallConn()
		if err != nil {
			return false
		}
		var sendErr error
		if err := raw.Control(func(fd uintptr) {
			_, _, sendErr = c.WriteMsgUnix([]byte(addr), syscall.UnixRights(int(fd)), nil)
		}); err != nil || sendErr != nil {
			return false
		}
		ack := make([]byte, 1)
		n, _ := c.Read(ack)
		return n == 1 && ack[0] == 'y'
	}, nil
}

// take is the socket a stopping relay offers, when it listens on addr; nil when none is
// offered, and this relay listens afresh.
func take(dir, addr string) net.Listener {
	path, err := handoffPath(dir)
	if err != nil {
		return nil
	}
	c, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return nil
	}
	defer func() { _ = c.Close() }()
	uc := c.(*net.UnixConn)
	_ = uc.SetDeadline(time.Now().Add(2 * time.Second))
	buf, oob := make([]byte, 512), make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil
	}
	f := receivedFile(oob[:oobn])
	if f == nil {
		_, _ = uc.Write([]byte{'n'})
		return nil
	}
	defer func() { _ = f.Close() }()
	// A relay moved to another address listens there; the old socket closes with its relay.
	if string(buf[:n]) != addr {
		_, _ = uc.Write([]byte{'n'})
		return nil
	}
	ln, err := net.FileListener(f)
	if err != nil {
		_, _ = uc.Write([]byte{'n'})
		return nil
	}
	_, _ = uc.Write([]byte{'y'})
	return ln
}

// receivedFile is the one descriptor oob carries; any others are closed.
func receivedFile(oob []byte) *os.File {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	var f *os.File
	for i := range msgs {
		fds, err := syscall.ParseUnixRights(&msgs[i])
		if err != nil {
			continue
		}
		for _, fd := range fds {
			syscall.CloseOnExec(fd)
			if f == nil {
				f = os.NewFile(uintptr(fd), "relay listener")
			} else {
				_ = syscall.Close(fd)
			}
		}
	}
	return f
}
