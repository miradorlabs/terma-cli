package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// `notify` is a top-level key, which TOML requires before any table header, so it is
// spliced line by line rather than through the [otel] merge.

const (
	codexNotifyKey   = "notify"
	codexNotifyState = "codex-notify.json"
)

// notifyCommand is the argv Codex runs; the JSON payload is appended.
var notifyCommand = []string{"terma", "hook", "codex-notify"}

// notifyStatus reports whether config.toml routes notify to terma.
type notifyStatus struct {
	ConfigPath string
	Terma      bool
}

// notifySetting inspects the notify setting.
func (c exporter) notifySetting() (notifyStatus, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return notifyStatus{}, err
	}
	st := notifyStatus{ConfigPath: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	// Parsed whole: a multiline notify array would line-scan as a bare `[`.
	var doc struct {
		Notify []string `toml:"notify"`
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return st, fmt.Errorf("parse Codex config: %w", err)
	}
	st.Terma = slices.Contains(doc.Notify, "terma") && slices.Contains(doc.Notify, "codex-notify")
	return st, nil
}

// RemoveNotifier restores the notifier that preceded Terma's turn capture.
func (c exporter) RemoveNotifier() (bool, error) { return c.removeNotify() }

// codexNotifyRecord keeps one displaced notifier per Codex config file, so a second
// CODEX_HOME cannot overwrite the first's.
type codexNotifyRecord struct {
	Chains map[string][]string `json:"chains,omitempty"`
}

// codexNotifyStatePath is the record under terma's config directory dir.
func codexNotifyStatePath(dir string) string {
	return filepath.Join(dir, config.SetupDir, codexNotifyState)
}

func loadCodexNotifyRecord(dir string) (*codexNotifyRecord, string, error) {
	path := codexNotifyStatePath(dir)
	rec := &codexNotifyRecord{Chains: map[string][]string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return rec, path, nil
	}
	if err != nil {
		return nil, path, err
	}
	if err := json.Unmarshal(b, rec); err != nil {
		return nil, path, err
	}
	if rec.Chains == nil {
		rec.Chains = map[string][]string{}
	}
	return rec, path, nil
}

func (rec *codexNotifyRecord) save(path string) error {
	if len(rec.Chains) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return config.WriteJSON(path, rec, harness.SettingsMode)
}

// updateCodexNotifyRecord is the one way the record changes, under a lock: it is one file
// for every Codex config, and unlocked concurrent teardowns would lose each other's change.
func updateCodexNotifyRecord(dir string, edit func(*codexNotifyRecord)) error {
	path := codexNotifyStatePath(dir)
	return flock.Locked(path, harness.RecordLockWait, func() error {
		rec, _, err := loadCodexNotifyRecord(dir)
		if err != nil {
			return err
		}
		edit(rec)
		return rec.save(path)
	})
}

func clearCodexNotifyChain(dir, configPath string) error {
	return updateCodexNotifyRecord(dir, func(rec *codexNotifyRecord) {
		delete(rec.Chains, configPath)
	})
}

// writeCodexConfig writes spliced config only if it still parses, and where and as
// tomlFile writes: through a symlinked config.toml, keeping a mode tighter than 0600.
func writeCodexConfig(path string, out []byte) error {
	var probe map[string]any
	if err := toml.Unmarshal(out, &probe); err != nil {
		return fmt.Errorf("refusing to write malformed Codex config: %w", err)
	}
	writePath, _, err := config.ResolveWritePath(path)
	if err != nil {
		return err
	}
	mode := harness.SettingsMode
	if info, err := os.Stat(writePath); err == nil && info.Mode().Perm()&0o077 == 0 {
		mode = info.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(writePath), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(writePath), err)
	}
	return config.WriteFileAtomic(writePath, out, mode)
}

func loadCodexNotifyChain(dir string) ([]string, error) {
	current, err := (exporter{}).ConfigPath()
	if err != nil {
		return nil, err
	}
	return loadCodexNotifyChainFor(dir, current)
}

func loadCodexNotifyChainFor(dir, configPath string) ([]string, error) {
	rec, _, err := loadCodexNotifyRecord(dir)
	if err != nil {
		return nil, err
	}
	return rec.Chains[configPath], nil
}

// runPreviousNotify forwards the payload to the notifier preserved under dir, best effort.
func runPreviousNotify(ctx context.Context, dir, payload string) error {
	previous, err := loadCodexNotifyChain(dir)
	if err != nil || len(previous) == 0 {
		return err
	}
	if isTermaNotify(previous) {
		return errors.New("refusing recursive Codex notify chain")
	}
	cmd := exec.CommandContext(ctx, previous[0], append(previous[1:], payload)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	return cmd.Run()
}

// removeNotify removes terma's notify entry and restores the notifier it displaced.
func (c exporter) removeNotify() (changed bool, err error) {
	st, err := c.notifySetting()
	if err != nil || !st.Terma {
		return false, err
	}
	data, _ := os.ReadFile(st.ConfigPath)
	previous, stateErr := loadCodexNotifyChainFor(c.dir, st.ConfigPath)
	if stateErr != nil {
		return false, fmt.Errorf("read previous Codex notify program: %w", stateErr)
	}
	line := ""
	if len(previous) > 0 {
		line = codexNotifyKey + " = " + renderStringArray(previous)
	}
	out := replaceTopLevel(data, codexNotifyKey, line)
	if err := writeCodexConfig(st.ConfigPath, out); err != nil {
		return false, err
	}
	if err := clearCodexNotifyChain(c.dir, st.ConfigPath); err != nil {
		return true, fmt.Errorf("remove Codex notify record: %w", err)
	}
	return true, nil
}

// isTermaNotify matches the exact argv prefix, not any path containing these words.
func isTermaNotify(argv []string) bool {
	return len(argv) >= len(notifyCommand) && slices.Equal(argv[:len(notifyCommand)], notifyCommand)
}

func tomlAssignment(line string) (key, value string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	eq := strings.Index(trimmed, "=")
	if eq <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(trimmed[:eq])
	value = strings.TrimSpace(trimmed[eq+1:])
	if strings.ContainsAny(key, " \t\"'") {
		return "", "", false
	}
	return key, value, true
}

// replaceTopLevel sets or, given an empty line, removes a top-level key, keeping every
// other byte; a new key goes before the first table header.
func replaceTopLevel(data []byte, key, line string) []byte {
	lines := strings.Split(string(data), "\n")
	var out []string
	inserted := false
	seenTable := false
	dropInsertedBlank := false
	dropArrayDepth := 0
	for _, l := range lines {
		if dropArrayDepth > 0 {
			dropArrayDepth += bracketDelta(l)
			continue
		}
		if dropInsertedBlank {
			dropInsertedBlank = false
			if strings.TrimSpace(l) == "" {
				continue
			}
		}
		trimmed := strings.TrimSpace(l)
		if !seenTable && strings.HasPrefix(trimmed, "[") {
			seenTable = true
			if !inserted && line != "" {
				out = append(out, line, "")
				inserted = true
			}
		}
		if !seenTable {
			if k, v, ok := tomlAssignment(l); ok && k == key {
				if line != "" && !inserted {
					out = append(out, line)
					inserted = true
				} else if line == "" {
					// The separator inserted with the key goes with it, so
					// removal restores the file byte for byte.
					dropInsertedBlank = true
				}
				dropArrayDepth = bracketDelta(v)
				continue // drop the old assignment (and its marker comment)
			}
		}
		out = append(out, l)
	}
	if !inserted && line != "" {
		for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			out = out[:len(out)-1]
		}
		out = append(out, line)
	}
	result := strings.Join(out, "\n")
	if !bytes.HasSuffix([]byte(result), []byte("\n")) {
		result += "\n"
	}
	return []byte(result)
}

// bracketDelta ignores brackets inside strings and trailing comments.
func bracketDelta(s string) int {
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' && quote == '"' {
				i++ // skip the escaped byte in a basic string
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '#':
			return depth // the rest of the line is a comment
		case '[':
			depth++
		case ']':
			depth--
		}
	}
	return depth
}

func renderStringArray(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = quoteTOMLString(v)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
