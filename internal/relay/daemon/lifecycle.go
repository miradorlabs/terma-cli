package daemon

import (
	"errors"
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

// Stop asks a running relay to stop and waits for its lock, so a changed address or token
// takes effect; it also returns once another relay, such as the service's waiting behind
// it, has taken over. While its setup remains, the relay hands its socket to the next
// relay (handoff.go), so the port stays bound throughout.
func Stop(dir string) {
	pid := recordedPID(dir)
	if pid <= 1 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	// The request comes first, so the relay knows terma stopped it and hands its socket on;
	// Windows signals nothing but a kill, which would skip delivery, so there it is all the relay sees.
	asked := config.WriteFileAtomicNoSync(filepath.Join(dir, StopFile), []byte(strconv.Itoa(pid)+"\n"), 0o600) == nil
	if proc.Signal(syscall.SIGTERM) != nil && !asked {
		return
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		unlock, err := flock.TryLock(filepath.Join(dir, LockFile))
		if err == nil {
			unlock()
			return
		}
		// A relay writes its pid only once it holds the lock and listens.
		if now := recordedPID(dir); flock.IsBusy(err) && now > 1 && now != pid {
			return
		}
	}
}

// recordedPID is the pid the running relay recorded, 0 when none.
func recordedPID(dir string) int {
	data, err := os.ReadFile(filepath.Join(dir, PIDFile))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
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
	if start(dir) != nil {
		return
	}
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

// SpawnSuccessor starts the relay that takes a stopping relay's socket once it releases the
// lock: an on-demand relay, which steps aside for the service's should that start.
func SpawnSuccessor(dir string) error {
	return start(dir, "--successor")
}

// start starts `terma relay run --quiet` detached, logging to the relay's log.
func start(dir string, args ...string) error {
	exe, err := os.Executable()
	// A <package>.test binary is no relay, and would only fail on the flags.
	if err != nil || strings.HasSuffix(filepath.Base(exe), ".test") {
		return errors.New("relay: no terma binary to start")
	}
	proc := spawnCommand(exe, dir, args...)
	procinfo.Detach(proc)
	out := openDaemonLog(dir)
	if out != nil {
		proc.Stdout, proc.Stderr = out, out
	}
	err = proc.Start()
	if out != nil {
		_ = out.Close()
	}
	if err != nil {
		return err
	}
	return proc.Process.Release()
}

// spawnCommand is the hook-started relay. A hook's environment is not the developer's (it
// may lack TERMA_ENV), so the relay runs in the one install recorded, where there is one.
func spawnCommand(exe, dir string, args ...string) *exec.Cmd {
	proc := exec.Command(exe, append([]string{"relay", "run", "--quiet"}, args...)...)
	if env, ok := RecordedEnv(dir); ok {
		proc.Env = withRelayEnv(os.Environ(), env)
	}
	proc.Stdin, proc.Stdout, proc.Stderr = nil, nil, nil
	return proc
}

// StartWait bounds how long a hook that started the relay waits for it to listen.
const StartWait = time.Second

// ServiceStartWait bounds how long a developer's command waits for the service's relay to
// listen before starting one itself: launchd and systemd start it in well under a second.
const ServiceStartWait = 5 * time.Second

// AwaitRelay waits up to wait for a relay to hold dir's lock and answer on its address,
// reporting whether one does. Starting another relay before the service's has taken the
// lock would leave the service waiting behind it.
func AwaitRelay(dir string, wait time.Duration) bool {
	addr := Addr(dir)
	for deadline := time.Now().Add(wait); ; time.Sleep(20 * time.Millisecond) {
		if Running(dir) {
			if conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
				_ = conn.Close()
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}
