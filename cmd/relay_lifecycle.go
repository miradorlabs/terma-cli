package cmd

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
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// relayRetryAfter is how long hooks leave a failed start before trying again.
const relayRetryAfter = time.Minute

// stopRelay asks a running relay to stop (SIGTERM through its pid file) and waits for
// its lock, so a setup that changed the address or token takes effect.
func stopRelay(dir string) {
	data, err := os.ReadFile(filepath.Join(dir, relayPIDFile))
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
		// Windows signals nothing but a kill, which would skip the relay's delivery of
		// what it accepted: ask through the stop file instead.
		if config.WriteFileAtomicNoSync(filepath.Join(dir, relayStopFile), []byte(strconv.Itoa(pid)+"\n"), 0o600) != nil {
			return
		}
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if unlock, err := flock.TryLock(filepath.Join(dir, relayLockFile)); err == nil {
			unlock()
			return
		}
	}
}

// spawnRelay starts `terma relay run` detached, unless one is already running. A hook
// calls it after claiming a session, so the relay is up before the session's first
// export in the common case and restarted if it idled out; two hooks racing here both
// start one and the second exits at its lock.
func spawnRelay() {
	dir, err := claim.Dir()
	if err != nil {
		return
	}
	unlock, err := flock.TryLock(filepath.Join(dir, relayLockFile))
	if err != nil {
		return // running (or unlockable: nothing to do from a hook either way)
	}
	unlock()
	// One that just failed to start (its port taken) is not retried by every hook:
	// each would start a process that fails the same way.
	if info, err := os.Stat(filepath.Join(dir, relayErrorFile)); err == nil && time.Since(info.ModTime()) < relayRetryAfter {
		return
	}
	exe, err := os.Executable()
	// A package's tests run as <package>.test: that binary is no relay, and would only
	// fail on the flags while the caller waited for it to listen.
	if err != nil || strings.HasSuffix(filepath.Base(exe), ".test") {
		return
	}
	proc := exec.Command(exe, "relay", "run", "--quiet")
	proc.Stdin, proc.Stdout, proc.Stderr = nil, nil, nil
	detach(proc)
	if err := proc.Start(); err != nil {
		return
	}
	_ = proc.Process.Release()
	// Wait, briefly, until it listens. The hook runs before its turn does, so an agent
	// that exports as soon as the hook returns — Claude Code 2.1.280 did, right after
	// UserPromptSubmit — finds the relay up instead of a refused connection it will not
	// retry. Only a hook that had to start the relay waits; the others return above.
	addr := relayAddr(dir)
	for deadline := time.Now().Add(relayStartWait); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
			_ = conn.Close()
			return
		}
	}
}

// relayStartWait bounds how long a hook that started the relay waits for it to listen.
const relayStartWait = time.Second
