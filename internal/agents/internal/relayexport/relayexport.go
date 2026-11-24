// Package relayexport is what the agents' relay exporters share.
package relayexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// Native points an agent's own OTLP exporter at the relay with all content; the relay
// withholds it per the team's policy.
func Native(h harness.Harness, cfg agents.RelayConfig) (agents.RelayResult, error) {
	return agents.RelayResult{}, h.Connect(harness.Exporter{Endpoint: cfg.Endpoint, APIKey: cfg.Token, Signals: harness.AllSignals}, true)
}

// Unnative undoes Native, restoring the agent's own exporter settings from its journal,
// and names any it did not restore because they changed since.
func Unnative(h harness.Harness) (agents.RelayResult, error) {
	result, err := h.Disconnect()
	if err != nil || result.Removed+result.Restored+len(result.Skipped) == 0 {
		return agents.RelayResult{}, err
	}
	path, err := h.ConfigPath()
	if err != nil {
		return agents.RelayResult{}, err
	}
	var out agents.RelayResult
	if result.Removed+result.Restored > 0 {
		out.Paths = []string{path}
	}
	for _, key := range result.Skipped {
		out.Notes = append(out.Notes, fmt.Sprintf("Did not restore %s in %s: it was changed after setup. Check it holds what you want.",
			key, output.TildePath(path)))
	}
	return out, nil
}

// WithTemplateLock serializes setup, teardown, and refresh on a template path using a
// canonicalized lock key in the temp directory (keeping HOME and extension dirs clean).
func WithTemplateLock(path string, fn func() error) error {
	key, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		key = filepath.Clean(path)
	}
	for cur, tail := key, []string(nil); ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			key = resolved
			break
		}
		if parent := filepath.Dir(cur); parent == cur {
			break
		} else {
			tail, cur = append(tail, filepath.Base(cur)), parent
		}
	}
	sum := sha256.Sum256([]byte(key))
	ctx, cancel := context.WithTimeout(context.Background(), harness.RecordLockWait)
	defer cancel()
	unlock, err := flock.Lock(ctx, filepath.Join(os.TempDir(), "terma-template-"+hex.EncodeToString(sum[:16])+".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// WriteOwn atomically writes a terma-owned extension or plugin template under its lock.
func WriteOwn(path string, data []byte, perm fs.FileMode) error {
	return WithTemplateLock(path, func() error {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		return config.WriteFileAtomic(path, data, perm)
	})
}

// RemoveOwn deletes the file or directory terma wrote as an agent's relay exporter.
// Optional lockPaths override the lock key (e.g. a directory holding an entry file).
func RemoveOwn(path string, lockPaths ...string) (agents.RelayResult, error) {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return agents.RelayResult{}, nil
	}
	lockKey := path
	if len(lockPaths) > 0 && lockPaths[0] != "" {
		lockKey = lockPaths[0]
	}
	var removed bool
	err := WithTemplateLock(lockKey, func() error {
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		removed = true
		return nil
	})
	if err != nil || !removed {
		return agents.RelayResult{}, err
	}
	return agents.RelayResult{Paths: []string{path}}, nil
}

// RefreshTemplate re-splices an installed extension's configuration into this build's template,
// leaving absent, unconfigured, or user-owned files and the file mode alone.
func RefreshTemplate(path string, render func(existing []byte) ([]byte, bool, error)) (string, bool, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return path, false, nil
	}
	var changed bool
	err := WithTemplateLock(path, func() error {
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		next, ok, err := render(data)
		if err != nil || !ok || bytes.Equal(next, data) {
			return err
		}
		if err := config.WriteFileAtomic(path, next, info.Mode().Perm()); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return path, changed, err
}

// NativePointed reports whether h's exporter sends to the relay at addr.
func NativePointed(h harness.Harness, addr string) bool {
	st, err := h.Status()
	if err != nil || !st.Connected {
		return false
	}
	ep := strings.TrimRight(st.Endpoint, "/")
	return ep == "http://"+addr || strings.HasPrefix(ep, "http://"+addr+"/")
}

// Headers authenticate an extension's exports to the relay.
func Headers(cfg agents.RelayConfig) map[string]string {
	return map[string]string{"Authorization": "Bearer " + cfg.Token}
}

// Own is embedded by an agent whose relay exporter is terma's own extension, which no
// settings file can say is pointed at the relay.
type Own struct{}

// RelayPointed is unknown: the exporter is terma's own.
func (Own) RelayPointed(string) (bool, bool) { return false, false }
