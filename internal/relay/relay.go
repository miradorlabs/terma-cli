// Package relay is terma's loopback OTLP relay: every agent's native export is pointed at
// it once, machine-wide, and it delivers each record to the project of the repository its
// session ran in (docs/RELAY.md). This file is the contract the rest of terma builds
// against — where the relay listens, how an agent authenticates to it, where hooks record
// a session's directory, and how the service is installed. The server, router and
// forwarder live beside it.
package relay

import (
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

	"github.com/miradorlabs/terma-cli/internal/config"
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

// RecordSession notes the directory a session runs in, for the relay to route its
// records by. A session-start hook calls it; it is one small unsynced file write, so it
// stays inside a hook's budget, and it never creates terma's config directory just to
// record a session (no relay has been set up there to read it).
func RecordSession(id, cwd string) error {
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
	return config.WriteFileAtomicNoSync(filepath.Join(sessions, id), []byte(cwd+"\n"), 0o600)
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
