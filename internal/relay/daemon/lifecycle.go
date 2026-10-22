package daemon

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

// RetryAfter is how long hooks leave a failed start before trying again.
const RetryAfter = time.Minute

// Stop asks a running relay to stop and waits for its lock, so a changed address or token
// takes effect; it also returns once another relay, such as the service's waiting behind
// it, has taken over. Only the relay holding the lock is asked (relayPID): the record a
// relay that crashed, or ran before a reboot, left behind names a pid another process may
// have now.
func Stop(dir string) {
	pid := relayPID(dir)
	if pid == 0 {
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
		unlock, err := flock.TryLock(filepath.Join(dir, LockFile))
		if err == nil {
			unlock()
			return
		}
		// A relay writes its record only once it holds the lock and listens.
		if now := recordedPID(dir); flock.IsBusy(err) && now > 1 && now != pid {
			return
		}
	}
}

// lockSettle is how long relayPID waits to see the lock still held.
const lockSettle = 25 * time.Millisecond

// settle is relayPID's wait; tests act in it.
var settle = func() { time.Sleep(lockSettle) }

// relayPID is the pid the relay holding dir's lock recorded, 0 when none holds it. A held
// lock alone does not tie the record to its holder, so the record is read again once the
// lock has been held for lockSettle: a relay that has just taken it clears a dead relay's
// record first (Run), and a probe holds it for an instant only.
func relayPID(dir string) int {
	pid := recordedPID(dir)
	if pid <= 1 || !Running(dir) {
		return 0
	}
	settle()
	if !Running(dir) || recordedPID(dir) != pid {
		return 0
	}
	return pid
}

// recordedPID is the pid the running relay recorded, 0 when none.
func recordedPID(dir string) int {
	data, err := os.ReadFile(filepath.Join(dir, RunFile))
	if err != nil {
		return 0
	}
	var info RunInfo
	if json.Unmarshal(data, &info) != nil {
		return 0
	}
	return info.PID
}

// Spawn starts `terma relay run` detached for the state directory stateDir unless one is
// running; of two racing hooks, the second's relay exits at its lock. A running relay of an
// earlier release than version is asked to make way, as the next command of a daemon-backed
// CLI restarts a daemon older than itself.
func Spawn(stateDir, version string) { SpawnFrom("", stateDir, version) }

// SpawnFrom is Spawn running the terma at exe, this process's own when "", the follower
// Supersede may start included: a relay whose package manager upgraded it starts its
// successor from the link that manager moved to the new release, never the versioned path it
// ran from, which the upgrade may have removed. version "" asks no relay to make way.
func SpawnFrom(exe, stateDir, version string) {
	dir := claim.Dir(stateDir)
	unlock, err := flock.TryLock(filepath.Join(dir, LockFile))
	if flock.IsBusy(err) {
		supersede(exe, stateDir, version)
	}
	if err != nil {
		return
	}
	unlock()
	// A relay that just failed to start is not retried by every hook: each would fail the same way.
	if info, err := os.Stat(filepath.Join(dir, ErrorFile)); err == nil && time.Since(info.ModTime()) < RetryAfter {
		return
	}
	if start(dir, exe) != nil {
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

// Supersede asks the relay running for the state directory stateDir to make way for this
// terma (version), and makes sure something takes its place.
//
// It asks only a relay of an earlier release, and a service's relay only when the service
// runs this binary: a service of another install would start the earlier release again, at
// every hook. A newer relay is left alone, so two installs never take turns.
//
// What takes the place of the relay making way: for the service's, the service manager, and
// for any other, the service's relay when one of this terma waits for the lock already (it
// holds ServiceWaitFile), or else the follower Supersede starts. The relay making way is the
// earlier release and starts nothing itself, and an agent exporting before the next hook
// would find nothing listening.
func Supersede(stateDir, version string) { supersede("", stateDir, version) }

// supersede is Supersede with the follower running the terma at exe, this process's own
// when "" (SpawnFrom).
func supersede(exe, stateDir, version string) {
	dir := claim.Dir(stateDir)
	running, ok := RunningRelay(dir)
	if !ok || !selfupdate.Newer(running.Version, version) {
		return
	}
	service := running.Launch == LaunchService
	ours := serviceRunsThis(stateDir)
	if service && !ours {
		return
	}
	_ = config.WriteFileAtomicNoSync(filepath.Join(dir, ReplaceFile), []byte(strconv.Itoa(running.PID)+"\n"), 0o600)
	if !service && (!ours || !serviceWaiting(dir)) {
		follow(dir, exe)
	}
}

// serviceWaiting reports whether the service's relay waits for the lock in dir (ServiceWaitFile).
func serviceWaiting(dir string) bool {
	unlock, err := flock.TryLock(filepath.Join(dir, ServiceWaitFile))
	if err == nil {
		unlock()
	}
	return flock.IsBusy(err)
}

// follow starts the relay that waits to take over from the one asked to make way, the terma
// at exe (this process's own when ""), unless one waits already. It does not wait: the relay
// making way still listens.
var follow = func(dir, exe string) {
	unlock, err := flock.TryLock(filepath.Join(dir, FollowLockFile))
	if err != nil {
		return
	}
	unlock()
	_ = start(dir, exe, "--follow")
}

// serviceRunsThis reports whether the installed service starts this terma.
var serviceRunsThis = func(stateDir string) bool { return CheckService(stateDir).Current }

// start starts `terma relay run --quiet` detached with args, logging to the relay's log: the
// terma at exe, or this process's own when exe is "".
func start(dir, exe string, args ...string) error {
	var err error
	if exe == "" {
		exe, err = os.Executable()
	}
	// A test binary is no relay: started as one, it would run its package's tests detached,
	// holding the relay's log open.
	if err != nil || testing.Testing() {
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
	proc := exec.Command(exe, append([]string{"relay", "run", "--quiet", "--launch", "hook"}, args...)...)
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
