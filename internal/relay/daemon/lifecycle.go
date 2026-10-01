package daemon

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// RetryAfter is how long hooks leave a failed start before trying again.
const RetryAfter = time.Minute

// Stop asks a running relay to stop and waits for its lock, so a changed address or token takes effect.
func Stop(dir string) {
	data, err := os.ReadFile(filepath.Join(dir, PIDFile))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if proc.Signal(syscall.SIGTERM) != nil {
		// Windows signals nothing but a kill, which would skip delivery: ask through the stop file.
		if config.WriteFileAtomicNoSync(filepath.Join(dir, StopFile), []byte(strconv.Itoa(pid)+"\n"), 0o600) != nil {
			return
		}
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if unlock, err := flock.TryLock(filepath.Join(dir, LockFile)); err == nil {
			unlock()
			return
		}
	}
}

// Spawn starts `terma relay run` detached unless one is running; of two racing hooks, the
// second's relay exits at its lock.
func Spawn() {
	dir, err := claim.Dir()
	if err != nil {
		return
	}
	unlock, err := flock.TryLock(filepath.Join(dir, LockFile))
	if err != nil {
		return
	}
	unlock()
	// A relay that just failed to start is not retried by every hook: each would fail the same way.
	if info, err := os.Stat(filepath.Join(dir, ErrorFile)); err == nil && time.Since(info.ModTime()) < RetryAfter {
		return
	}
	exe, err := os.Executable()
	// A <package>.test binary is no relay, and would only fail on the flags.
	if err != nil || strings.HasSuffix(filepath.Base(exe), ".test") {
		return
	}
	proc := exec.Command(exe, "relay", "run", "--quiet")
	proc.Stdin, proc.Stdout, proc.Stderr = nil, nil, nil
	procinfo.Detach(proc)
	if err := proc.Start(); err != nil {
		return
	}
	_ = proc.Process.Release()
	// Wait until it listens: an agent may export as soon as the hook returns, and never
	// retries a refused connection.
	addr := Addr(dir)
	for deadline := time.Now().Add(StartWait); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
			_ = conn.Close()
			return
		}
	}
}

// StartWait bounds how long a hook that started the relay waits for it to listen.
const StartWait = time.Second
