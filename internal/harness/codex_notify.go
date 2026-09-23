package harness

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
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	toml "github.com/pelletier/go-toml/v2"
)

// Codex has no file-edit hooks, but it can run a `notify` program at the end of
// every turn with a JSON description of the turn (thread id, cwd, ...). That is
// enough to announce a session, so terma's Codex adapter is a notify entry in
// config.toml pointing at `terma hook codex-notify`.
//
// `notify` is a top-level key, which TOML requires to appear before any table
// header, so it is spliced line-by-line rather than through the [otel] merge.

const (
	codexNotifyKey    = "notify"
	codexNotifyMarker = "# managed by terma (codex-notify adapter)"
	codexNotifyState  = "codex-notify.json"
)

// CodexNotifyCommand is the argv Codex runs; the JSON payload is appended.
var CodexNotifyCommand = []string{"terma", "hook", "codex-notify"}

// CodexNotifyStatus reports whether config.toml routes notify to terma, or to
// something else the user configured.
type CodexNotifyStatus struct {
	ConfigPath string
	Configured bool // notify is set at all
	Terma      bool // notify is terma's
	Value      string
}

// CodexNotify inspects the notify setting.
func (c Codex) CodexNotify() (CodexNotifyStatus, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return CodexNotifyStatus{}, err
	}
	st := CodexNotifyStatus{ConfigPath: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	// Parse the whole document rather than scanning a single line: notify can be a
	// multiline array, which a line scan reads as the bare `[`.
	var doc struct {
		Notify []string `toml:"notify"`
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return st, fmt.Errorf("parse Codex config: %w", err)
	}
	if len(doc.Notify) > 0 {
		st.Configured = true
		st.Value = renderStringArray(doc.Notify)
		st.Terma = slices.Contains(doc.Notify, "terma") && slices.Contains(doc.Notify, "codex-notify")
	}
	return st, nil
}

// codexNotifyRecord is what terma remembers about the notifiers it displaced: one chain
// per Codex config file. It was a single record, so connecting under a second
// CODEX_HOME overwrote the first config's notifier — or, installing over no notifier
// there, deleted it — and the later disconnect had nothing to put back.
type codexNotifyRecord struct {
	// Chains maps a Codex config path to the notifier argv terma displaced there.
	Chains map[string][]string `json:"chains,omitempty"`

	// ConfigPath and Previous are the single-record shape, read and folded into Chains.
	// An empty ConfigPath is the first chaining build's, which bound the record to
	// nothing; it is kept under "" and answers for any config that has none of its own.
	ConfigPath string   `json:"config_path,omitempty"`
	Previous   []string `json:"previous,omitempty"`
}

func codexNotifyStatePath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, codexNotifyState), nil
}

func parseCodexNotify(value string) ([]string, error) {
	var doc struct {
		Notify []string `toml:"notify"`
	}
	if err := toml.Unmarshal([]byte("notify = "+value), &doc); err != nil {
		return nil, err
	}
	if len(doc.Notify) == 0 {
		return nil, errors.New("notify command is empty")
	}
	return doc.Notify, nil
}

// loadCodexNotifyRecord reads the record, folding the legacy shape into Chains. A
// missing file is an empty record.
func loadCodexNotifyRecord() (*codexNotifyRecord, string, error) {
	path, err := codexNotifyStatePath()
	if err != nil {
		return nil, "", err
	}
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
	if len(rec.Previous) > 0 {
		if _, taken := rec.Chains[rec.ConfigPath]; !taken {
			rec.Chains[rec.ConfigPath] = rec.Previous
		}
	}
	rec.ConfigPath, rec.Previous = "", nil
	return rec, path, nil
}

// save writes the record, or removes the file once no chain is left in it.
func (rec *codexNotifyRecord) save(path string) error {
	if len(rec.Chains) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return config.WriteJSON(path, rec, settingsMode)
}

// recordLockWait bounds the wait for another terma's update of a displaced-settings
// record (the Codex notifier, the Claude status line). Only connect, install and
// disconnect write one — never a hook — so it may wait.
const recordLockWait = 5 * time.Second

// updateCodexNotifyRecord is the one way the record changes: load, edit, save, under a
// lock. It is a single file for every Codex config on the machine, so two connects
// under different CODEX_HOMEs each read it, set their own chain and renamed their copy
// back, and the later rename forgot the other config's notifier.
func updateCodexNotifyRecord(edit func(*codexNotifyRecord)) error {
	path, err := codexNotifyStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordLockWait)
	defer cancel()
	unlock, err := flock.Lock(ctx, path+".lock")
	if err != nil {
		return err
	}
	defer unlock()
	rec, _, err := loadCodexNotifyRecord()
	if err != nil {
		return err
	}
	edit(rec)
	return rec.save(path)
}

func saveCodexNotifyChain(configPath string, previous []string) error {
	return updateCodexNotifyRecord(func(rec *codexNotifyRecord) { rec.Chains[configPath] = previous })
}

// clearCodexNotifyChain forgets the notifier displaced at configPath, and only that one.
// The legacy unbound chain goes with it: it was this machine's one record, and leaving
// it would resurrect a notifier on the next disconnect of any config.
func clearCodexNotifyChain(configPath string) error {
	return updateCodexNotifyRecord(func(rec *codexNotifyRecord) {
		delete(rec.Chains, configPath)
		delete(rec.Chains, "")
	})
}

// writeCodexConfig writes spliced config only if it still parses as TOML, so a
// splice that mishandled an exotic value (e.g. a bracket inside a multiline string)
// fails cleanly instead of leaving config.toml corrupt.
//
// It writes where tomlFile writes and as tomlFile writes. The rename used to land on
// path itself at 0600: a `terma connect codex` that had just kept a symlinked
// ~/.codex/config.toml pointing into a dotfiles repository replaced the link with a
// regular file a moment later, and a file kept at 0400 was loosened.
func writeCodexConfig(path string, out []byte) error {
	var probe map[string]any
	if err := toml.Unmarshal(out, &probe); err != nil {
		return fmt.Errorf("refusing to write malformed Codex config: %w", err)
	}
	writePath, _, err := resolveWritePath(path)
	if err != nil {
		return err
	}
	mode := settingsMode
	if info, err := os.Stat(writePath); err == nil && info.Mode().Perm()&0o077 == 0 {
		mode = info.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(writePath), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(writePath), err)
	}
	return config.WriteFileAtomic(writePath, out, mode)
}

// InstallCodexNotify points notify at terma. When another notifier is already
// configured, terma records and chains it instead of making setup choose between
// funding capture and the user's existing integration.
func (c Codex) InstallCodexNotify() (changed bool, err error) {
	st, err := c.CodexNotify()
	if err != nil {
		return false, err
	}
	if st.Terma {
		return false, nil
	}
	if st.Configured {
		previous, err := parseCodexNotify(st.Value)
		if err != nil {
			return false, fmt.Errorf("parse existing Codex notify program: %w", err)
		}
		if err := saveCodexNotifyChain(st.ConfigPath, previous); err != nil {
			return false, fmt.Errorf("preserve existing Codex notify program: %w", err)
		}
	} else if err := clearCodexNotifyChain(st.ConfigPath); err != nil {
		// Installing over no notifier: drop any record from an earlier install so a
		// later disconnect does not resurrect a notifier this install never displaced.
		return false, fmt.Errorf("clear stale Codex notify record: %w", err)
	}
	data, _ := os.ReadFile(st.ConfigPath)
	line := codexNotifyKey + " = " + renderStringArray(CodexNotifyCommand) + " " + codexNotifyMarker
	out := replaceTopLevel(data, codexNotifyKey, line)
	if err := writeCodexConfig(st.ConfigPath, out); err != nil {
		return false, err
	}
	return true, nil
}

func loadCodexNotifyChain() ([]string, error) {
	current, err := (Codex{}).ConfigPath()
	if err != nil {
		return nil, err
	}
	return loadCodexNotifyChainFor(current)
}

// loadCodexNotifyChainFor reads the notifier a connect displaced at configPath. The
// legacy unbound chain answers only when configPath has none of its own, so that
// upgrading the first chaining build still restores the notifier it preserved.
func loadCodexNotifyChainFor(configPath string) ([]string, error) {
	rec, _, err := loadCodexNotifyRecord()
	if err != nil {
		return nil, err
	}
	if previous, ok := rec.Chains[configPath]; ok {
		return previous, nil
	}
	return rec.Chains[""], nil
}

// RunPreviousCodexNotify forwards the payload to the notifier setup preserved.
// It is best-effort: terma's capture must not be lost because a UI notifier fails.
func RunPreviousCodexNotify(ctx context.Context, payload string) error {
	previous, err := loadCodexNotifyChain()
	if err != nil || len(previous) == 0 {
		return err
	}
	// Recursive only if the preserved program is terma's own notify (exact argv
	// prefix), not merely any command whose path happens to contain these words.
	if isTermaNotify(previous) {
		return errors.New("refusing recursive Codex notify chain")
	}
	cmd := exec.CommandContext(ctx, previous[0], append(previous[1:], payload)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	return cmd.Run()
}

// RemoveCodexNotify removes terma's notify entry and restores the notifier it
// displaced, if any.
func (c Codex) RemoveCodexNotify() (changed bool, err error) {
	st, err := c.CodexNotify()
	if err != nil || !st.Terma {
		return false, err
	}
	data, _ := os.ReadFile(st.ConfigPath)
	previous, stateErr := loadCodexNotifyChainFor(st.ConfigPath)
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
	if err := clearCodexNotifyChain(st.ConfigPath); err != nil {
		return true, fmt.Errorf("remove Codex notify record: %w", err)
	}
	return true, nil
}

// isTermaNotify reports whether argv is terma's own notifier: the exact argv prefix,
// not merely a command whose path happens to contain these words.
func isTermaNotify(argv []string) bool {
	return len(argv) >= len(CodexNotifyCommand) && slices.Equal(argv[:len(CodexNotifyCommand)], CodexNotifyCommand)
}

// tomlAssignment parses `key = value` at the top level (bare keys only).
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

// replaceTopLevel sets (or, with an empty line, removes) a top-level key, keeping
// every other byte of the file. A new key goes just before the first table header
// so it stays top-level.
func replaceTopLevel(data []byte, key, line string) []byte {
	lines := strings.Split(string(data), "\n")
	var out []string
	inserted := false
	seenTable := false
	dropInsertedBlank := false
	dropArrayDepth := 0
	for _, l := range lines {
		if dropArrayDepth > 0 {
			// Drop the continuation lines of a multiline array whose opening line was
			// the assignment we removed, until its brackets close.
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
					// A newly inserted top-level key carries one separator line before
					// the first table. Remove that separator with the key so disconnect
					// restores the original file byte for byte.
					dropInsertedBlank = true
				}
				// A multiline array spills onto following lines; drop those too.
				dropArrayDepth = bracketDelta(v)
				continue // drop the old assignment (and its marker comment)
			}
		}
		out = append(out, l)
	}
	if !inserted && line != "" {
		// No table header: append at the end.
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

// bracketDelta is the net array-bracket depth a line adds, ignoring brackets inside
// strings or a trailing comment, so a multiline notify array can be tracked to close.
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
