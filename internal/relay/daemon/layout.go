package daemon

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// The files in the relay's directory.
const (
	AddrFile = "addr"
	// LockFile is held by the running relay for as long as it runs.
	LockFile  = "daemon.lock"
	StatsFile = "stats.json"
	// PrevStatsFile is the exit counters of the relay before the last one.
	PrevStatsFile = "stats-prev.json"
	ErrorFile     = "last-error"
	// StopFile asks the relay whose pid it holds to stop, since Windows has no SIGTERM.
	StopFile = "stop"
	// ReplaceFile asks the relay whose pid it holds to make way for a newer terma (Spawn).
	ReplaceFile = "replace"
	// FollowLockFile is held by the one relay waiting to take over from a relay asked to
	// make way (Config.Follow).
	FollowLockFile = "follow.lock"
	// ServiceWaitFile is held by the service's relay while it waits for another relay's lock,
	// so Supersede knows it will take over and starts no follower.
	ServiceWaitFile = "service-wait.lock"
	NoServiceFile   = "no-service"
	// EnvFile is the environment every relay of this state directory runs in (RecordEnv).
	EnvFile = "env.json"
	// RunFile describes the running relay, its pid included (RunningRelay).
	RunFile = "daemon.json"
	// MovedFile records the relay's last move off an address it could not listen on (LastMove).
	MovedFile = "moved.json"
	// ExportersLockFile is held while terma points the agents' exporters at the relay, by
	// setup and by a relay that moved, which may do it at once.
	ExportersLockFile = "exporters.lock"
)

// Dir is the relay's directory under terma's state directory stateDir, created if missing.
func Dir(stateDir string) (string, error) {
	dir := claim.Dir(stateDir)
	return dir, os.MkdirAll(dir, 0o700)
}

// Addr is the address the relay listens on: what setup recorded, else the default.
func Addr(dir string) string {
	if data, err := os.ReadFile(filepath.Join(dir, AddrFile)); err == nil {
		if a := strings.TrimSpace(string(data)); a != "" {
			return a
		}
	}
	return claim.DefaultAddr
}

// Token is the relay's local token under terma's state directory stateDir, which the agents'
// exporters present.
func Token(stateDir string) (string, error) {
	data, err := os.ReadFile(claim.TokenPath(stateDir))
	if err != nil {
		return "", errors.New("the relay is not set up on this machine — run `terma setup`")
	}
	return strings.TrimSpace(string(data)), nil
}

// Running reports whether a relay holds the relay directory dir's lock.
func Running(dir string) bool {
	unlock, err := flock.TryLock(filepath.Join(dir, LockFile))
	if err == nil {
		unlock()
	}
	return flock.IsBusy(err)
}

// RunInfo is what the running relay records about itself.
type RunInfo struct {
	PID int `json:"pid"`
	// Addr is the address it listens on, as the relay's directory records it (Serving).
	Addr string `json:"addr,omitempty"`
	// Environment is the backend environment it delivers to.
	Environment string `json:"environment"`
	// Launch is what started the relay: the service manager, a hook, or a developer.
	Launch Launch `json:"launch"`
	// Service is Launch == LaunchService, still written for a terma from before Launch,
	// and read from one: its record has no launch.
	Service bool `json:"service"`
	// Version is the terma it runs.
	Version string `json:"version,omitempty"`
}

// RunningRelay is what the relay holding dir's lock recorded about itself; false when none
// runs, or it is a terma from before the record.
func RunningRelay(dir string) (RunInfo, bool) {
	if !Running(dir) {
		return RunInfo{}, false
	}
	data, err := os.ReadFile(filepath.Join(dir, RunFile))
	if err != nil {
		return RunInfo{}, false
	}
	var info RunInfo
	if json.Unmarshal(data, &info) != nil || info.PID <= 0 {
		return RunInfo{}, false
	}
	if info.Launch == "" && info.Service {
		info.Launch = LaunchService
	}
	return info, true
}

// Serving is the address this state directory's relay listens on, and whether it is the
// relay's address (Addr): the relay holds the lock and recorded that it listens there. A
// port answering is no such proof, since any account on the machine may listen on a
// loopback port this relay is not holding. The lock is probed only once a live process's
// record is there, and Spawn and a starting relay clear a dead one's, so a relay still
// starting does not find the probe holding its lock. A relay from before relays recorded
// their address is taken at its lock.
func Serving(dir string) (string, bool) {
	addr := Addr(dir)
	data, err := os.ReadFile(filepath.Join(dir, RunFile))
	if err != nil {
		return addr, false
	}
	var info RunInfo
	if json.Unmarshal(data, &info) != nil || info.Addr != "" && info.Addr != addr || info.PID <= 0 || !procinfo.Alive(info.PID) {
		return addr, false
	}
	return addr, Running(dir)
}

// Squatted reports whether something else answers on addr while the relay is not running.
func Squatted(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
