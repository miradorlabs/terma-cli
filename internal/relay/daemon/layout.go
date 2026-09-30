package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// The files in the relay's state directory.
const (
	AddrFile  = "addr"
	LockFile  = "relay.lock"
	StatsFile = "stats.json"
	PIDFile   = "pid"
	ErrorFile = "last-error"
	// StopFile asks the relay whose pid it holds to stop: how Stop reaches a relay on
	// Windows, which has no SIGTERM. The relay looks every second.
	StopFile          = "stop"
	MachineIDFile     = "machine-id"
	NoServiceFile     = "no-service"
	SupervisorPIDFile = "supervisor.pid"
)

// Dir is the relay's state directory, created if missing.
func Dir() (string, error) {
	dir, err := claim.Dir()
	if err != nil {
		return "", err
	}
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

// Token is the relay's local token, which the agents' exporters present.
func Token() (string, error) {
	path, err := claim.TokenPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("the relay is not set up on this machine — run `terma relay setup`")
	}
	return strings.TrimSpace(string(data)), nil
}

// Running reports whether a relay holds the state directory's lock.
func Running(dir string) bool {
	unlock, err := flock.TryLock(filepath.Join(dir, LockFile))
	if err != nil {
		return true
	}
	unlock()
	return false
}

// Squatted reports whether something answers on addr while the relay is not running:
// the agents' exporters would be sending to it.
func Squatted(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Stats reads the running relay's counters, else those the last run left behind, and
// says whether one is running.
func Stats(dir string) (relay.Snapshot, bool, error) {
	var snap relay.Snapshot
	if !Running(dir) {
		data, err := os.ReadFile(filepath.Join(dir, StatsFile))
		if err != nil {
			return snap, false, nil
		}
		return snap, false, json.Unmarshal(data, &snap)
	}
	token, err := Token()
	if err != nil {
		return snap, true, err
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+Addr(dir)+"/stats", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return snap, true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return snap, true, fmt.Errorf("relay stats: HTTP %s", resp.Status)
	}
	body, _ := io.ReadAll(resp.Body)
	return snap, true, json.Unmarshal(body, &snap)
}
