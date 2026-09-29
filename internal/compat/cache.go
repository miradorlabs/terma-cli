package compat

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// Resolve symlinks so Homebrew upgrades and PATH changes select a different cache.
// Metadata invalidates an entry when an executable is updated in place, without
// reading or hashing a potentially large binary on every launch.
type executableIdentity struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified_ns"`
	Mode     uint32 `json:"mode"`
}

func binaryIdentity(binary string) (executableIdentity, error) {
	path, err := filepath.Abs(binary)
	if err != nil {
		return executableIdentity{}, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return executableIdentity{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return executableIdentity{}, err
	}
	return executableIdentity{Path: path, Size: info.Size(), Modified: info.ModTime().UnixNano(), Mode: uint32(info.Mode())}, nil
}

type capabilityCache struct {
	Schema   int                `json:"schema"`
	Binary   executableIdentity `json:"binary"`
	Version  string             `json:"harness_version"`
	NoDaemon *bool              `json:"no_daemon,omitempty"`
}

func capabilityCachePath(binary string) string {
	dir, err := config.Dir()
	if err != nil || binary == "" {
		return ""
	}
	return filepath.Join(dir, "cache", "compat-codex-cli", fmt.Sprintf("%x.json", sha256.Sum256([]byte(binary))))
}

func readCache(path string, identity executableIdentity) (capabilityCache, bool) {
	var c capabilityCache
	data, err := os.ReadFile(path)
	ok := err == nil && json.Unmarshal(data, &c) == nil && c.Schema == 1 && c.Binary == identity
	return c, ok
}

func saveCache(path string, c capabilityCache) {
	data, err := json.Marshal(c)
	if path != "" && err == nil && os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		_ = config.WriteFileAtomicNoSync(path, data, 0o600)
	}
}
