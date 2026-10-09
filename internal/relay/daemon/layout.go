package daemon

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
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
	NoServiceFile  = "no-service"
	// EnvFile is the environment every relay of this state directory runs in (RecordEnv).
	EnvFile = "env.json"
	// RunFile describes the running relay, its pid included (RunningRelay).
	RunFile = "daemon.json"
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

// Squatted reports whether something else answers on addr while the relay is not running.
func Squatted(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
