// Package relay is terma's loopback OTLP relay: every agent's native export is pointed at
// it once, machine-wide, and it delivers each record to the project of the repository its
// session ran in (docs/RELAY.md). This file is the contract the rest of terma builds
// against — where the relay listens, how an agent authenticates to it, where hooks record
// a session's directory, and how the service is installed. The server, router and
// forwarder live beside it.
package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// DefaultPort is the loopback port the relay listens on. It is deliberately not 4317 or
// 4318, which a developer's own collector is likely to hold.
const DefaultPort = 14318

const (
	dirName     = "relay"
	configName  = "relay.json"
	sessionsDir = "sessions"
)

// Config is the relay's machine-wide settings, written by `terma setup`.
type Config struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

// Endpoint is the OTLP/HTTP base URL agents export to; /v1/logs, /v1/traces and
// /v1/metrics are appended per signal.
func (c Config) Endpoint() string {
	return "http://127.0.0.1:" + strconv.Itoa(c.Port)
}

// Authorization is the header value an agent sends with every export.
func (c Config) Authorization() string {
	return "Bearer " + c.Token
}

// Dir is the relay's state directory under terma's config directory.
func Dir() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, dirName), nil
}

// ErrNotConfigured is returned by Load when setup has not written the relay's config.
var ErrNotConfigured = errors.New("relay is not configured")

// Load reads the relay's config, ErrNotConfigured when there is none.
func Load() (Config, error) {
	dir, err := Dir()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, configName))
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, ErrNotConfigured
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("read %s: %w", configName, err)
	}
	if c.Port <= 0 || c.Port > 65535 || c.Token == "" {
		return Config{}, fmt.Errorf("read %s: incomplete", configName)
	}
	return c, nil
}

// Ensure returns the relay's config, writing one with a fresh token on first use. An
// existing config is kept as it is, so agents configured against it keep working.
func Ensure() (Config, error) {
	c, err := Load()
	if err == nil || !errors.Is(err, ErrNotConfigured) {
		return c, err
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return Config{}, err
	}
	c = Config{Port: DefaultPort, Token: hex.EncodeToString(token)}
	dir, err := Dir()
	if err != nil {
		return Config{}, err
	}
	if err := config.WriteJSON(filepath.Join(dir, configName), c, 0o600); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Supported reports whether this platform can keep the relay running: launchd (macOS),
// systemd --user (Linux), or the Run key and `terma relay supervise` (Windows). Elsewhere
// agents export straight to Terma.
func Supported() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "linux" || runtime.GOOS == "windows"
}

// RecordSession notes that a session runs in cwd from now on, for the relay to route
// its records by. A session-start hook calls it — the repository's, and the global one
// that sees a session resumed elsewhere — so it is small: one read and, when the
// directory changed, one unsynced write, under a lock both hooks of a start may contend
// for. It never creates terma's config directory just to record a session (no relay has
// been set up there to read it).
func RecordSession(id, cwd string) error {
	return recordPlacement(id, cwd, time.Now())
}

// maxPlacements bounds a session's placement history.
const maxPlacements = 32

func recordPlacement(id, cwd string, at time.Time) error {
	if !validSessionID(id) || cwd == "" {
		return nil
	}
	dir, err := Dir()
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	sessions := filepath.Join(dir, sessionsDir)
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		return err
	}
	// A lock that cannot be had within the hook's budget falls through to the write:
	// losing a race costs one placement line, dropping the write loses the session.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if unlock, err := flock.Lock(ctx, filepath.Join(dir, "sessions.lock")); err == nil {
		defer unlock()
	}
	path := filepath.Join(sessions, id)
	existing := readLines(path)
	if n := len(existing); n > 0 && existing[n-1].value == cwd {
		return nil // still where it was: the earlier line's time stands
	}
	existing = append(existing, line{since: at, value: cwd})
	if len(existing) > maxPlacements {
		existing = existing[len(existing)-maxPlacements:]
	}
	var b strings.Builder
	for _, l := range existing {
		b.WriteString(formatSince(l.since) + "\t" + l.value + "\n")
	}
	return config.WriteFileAtomicNoSync(path, []byte(b.String()), 0o600)
}

// validSessionID admits the ids agents use (UUIDs, Codex's thread ids) and nothing that
// could name a path outside the sessions directory.
func validSessionID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
