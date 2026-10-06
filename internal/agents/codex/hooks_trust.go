package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// Codex skips a hook until the developer trusts that exact entry's hash in Codex, recorded
// under `[hooks.state]` in the user config as `<hooks.json path>:<event>:<group>:<hook>`,
// the event in snake_case. Terma approves, there, only the entries it writes itself, byte
// for byte (Dawson's call, 2026-10-02: setup and install are the developer's consent), and
// withdraws only the approvals it wrote once those entries go (syncHookTrust). Every other
// entry stays the developer's to review in Codex.
const (
	codexHooksStateTable = "hooks"
	codexHooksStateKey   = "state"
	codexTrustedHashKey  = "trusted_hash"
	codexHookEnabledKey  = "enabled"
)

// hookTrust is what the user's Codex config says about one hooks file.
type hookTrust struct {
	ConfigPath string
	// Entries is how many hooks from this file Codex has a record for.
	Entries int
	// Trusted is how many of those carry a trusted hash.
	Trusted  int
	Disabled int
	// TrustedKeys names the trusted entries ("<event>:<group>:<handler>"): an entry added to
	// an already-trusted file has no record and is skipped without a word.
	TrustedKeys map[string]bool
	// TrustedHashes are Codex's recorded hashes by entry key; a changed entry no longer matches.
	TrustedHashes map[string]string
}

// Reviewed reports whether Codex has any record for this file; a fresh clone has none.
func (t hookTrust) Reviewed() bool { return t.Entries > 0 }

// hookTrustFor reports what the user's Codex config records about the hooks file at
// hooksPath; an unreadable or absent config means nothing is trusted.
func (c exporter) hookTrustFor(hooksPath string) (hookTrust, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return hookTrust{}, err
	}
	trust := hookTrust{ConfigPath: path}
	f, err := loadTOML(path)
	if err != nil {
		return trust, err
	}
	hooks, _ := f.doc[codexHooksStateTable].(map[string]any)
	state, _ := hooks[codexHooksStateKey].(map[string]any)
	if len(state) == 0 {
		return trust, nil
	}
	// Codex records the path it loaded: compare resolved, then literal, so a symlinked
	// repository does not read as never trusted.
	prefixes := []string{hooksPath + ":"}
	if resolved, err := filepath.EvalSymlinks(hooksPath); err == nil && resolved != hooksPath {
		prefixes = append(prefixes, resolved+":")
	}
	for key, raw := range state {
		matched, name := false, ""
		for _, prefix := range prefixes {
			if after, ok := strings.CutPrefix(key, prefix); ok {
				matched, name = true, after
				break
			}
		}
		if !matched {
			continue
		}
		trust.Entries++
		entry, _ := raw.(map[string]any)
		if hash, ok := entry[codexTrustedHashKey].(string); ok && strings.TrimSpace(hash) != "" {
			trust.Trusted++
			if trust.TrustedKeys == nil {
				trust.TrustedKeys = map[string]bool{}
				trust.TrustedHashes = map[string]string{}
			}
			trust.TrustedKeys[name] = true
			trust.TrustedHashes[name] = hash
		}
		if enabled, ok := entry[codexHookEnabledKey].(bool); ok && !enabled {
			trust.Disabled++
		}
	}
	return trust, nil
}

// codexTrustState records the approvals terma wrote: by Codex config, each record's key and
// the hash terma wrote, so it withdraws only its own.
const codexTrustState = "codex-hook-trust.json"

type codexTrustRecord struct {
	Configs map[string]map[string]string `json:"configs,omitempty"`
}

// trustSync is what syncHookTrust changed.
type trustSync struct{ Approved, Withdrawn int }

// syncHookTrust brings the Codex config at configPath in step with the hooks file at
// hooksFile: each entry there byte for byte terma's (written with command) is approved, and
// an approval terma wrote for an entry no longer there is withdrawn. An entry switched off
// in Codex stays off, and an approval the developer gave stays theirs. Only those records'
// tables change, proven before writing to read as the original with just them changed.
func syncHookTrust(dir, configPath, hooksFile string, command func(event string) string) (trustSync, error) {
	var done trustSync
	statePath := codexTrustStatePath(dir)
	err := flock.Locked(statePath, harness.RecordLockWait, func() error {
		entries, err := ownEntries(hooksFile, command)
		if err != nil {
			return err
		}
		paths, err := pathSpellings(hooksFile)
		if err != nil {
			return err
		}
		want := map[string]string{}
		for _, path := range paths {
			for _, e := range entries {
				want[path+":"+e.Key()] = e.Hash
			}
		}
		ours := func(key string) bool {
			return slices.ContainsFunc(paths, func(p string) bool { return strings.HasPrefix(key, p+":") })
		}

		rec, err := loadCodexTrustRecord(statePath)
		if err != nil {
			return err
		}
		wrote := rec.Configs[configPath]
		if wrote == nil {
			wrote = map[string]string{}
		}

		raw, err := os.ReadFile(configPath)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("read %s: %w", configPath, err)
		}
		doc := map[string]any{}
		if err := toml.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("parse %s: %w", configPath, err)
		}
		if doc == nil {
			doc = map[string]any{}
		}
		state, err := trustTable(doc, configPath)
		if err != nil {
			return err
		}

		var changed []string
		for key, hash := range want {
			record, _ := state[key].(map[string]any)
			if enabled, ok := record[codexHookEnabledKey].(bool); ok && !enabled {
				continue
			}
			if record[codexTrustedHashKey] == hash {
				continue
			}
			if record == nil {
				record = map[string]any{}
			}
			record[codexTrustedHashKey] = hash
			state[key] = record
			wrote[key] = hash
			changed = append(changed, key)
			done.Approved++
		}
		for key, hash := range wrote {
			if !ours(key) {
				continue
			}
			if _, keep := want[key]; keep {
				continue
			}
			delete(wrote, key)
			// Approved again since, by the developer, for something else: theirs now.
			record, _ := state[key].(map[string]any)
			if record[codexTrustedHashKey] != hash {
				continue
			}
			delete(record, codexTrustedHashKey)
			if len(record) == 0 {
				delete(state, key)
			}
			changed = append(changed, key)
			done.Withdrawn++
		}
		if len(changed) > 0 {
			if err := writeTrustRecords(configPath, raw, doc, state, changed); err != nil {
				return err
			}
		}
		if len(wrote) == 0 {
			delete(rec.Configs, configPath)
		} else {
			if rec.Configs == nil {
				rec.Configs = map[string]map[string]string{}
			}
			rec.Configs[configPath] = wrote
		}
		return rec.save(statePath)
	})
	// Approvals per hooks file, not per spelling of its path.
	if paths, perr := pathSpellings(hooksFile); perr == nil {
		done.Approved /= len(paths)
		done.Withdrawn /= len(paths)
	}
	return done, err
}

// pathSpellings are the paths Codex may record the hooks file at path under: as given, and
// through a symlinked directory resolved. The directory, since the file may be gone.
func pathSpellings(path string) ([]string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	paths := []string{abs}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		if resolved := filepath.Join(dir, filepath.Base(abs)); resolved != abs {
			paths = append(paths, resolved)
		}
	}
	return paths, nil
}

// trustTable is doc's [hooks.state] table, made when absent.
func trustTable(doc map[string]any, configPath string) (map[string]any, error) {
	hooks, ok := doc[codexHooksStateTable].(map[string]any)
	if _, present := doc[codexHooksStateTable]; present && !ok {
		return nil, fmt.Errorf("%s: %q is not a table", configPath, codexHooksStateTable)
	}
	if hooks == nil {
		hooks = map[string]any{}
		doc[codexHooksStateTable] = hooks
	}
	state, ok := hooks[codexHooksStateKey].(map[string]any)
	if _, present := hooks[codexHooksStateKey]; present && !ok {
		return nil, fmt.Errorf("%s: %s.%s is not a table", configPath, codexHooksStateTable, codexHooksStateKey)
	}
	if state == nil {
		state = map[string]any{}
		hooks[codexHooksStateKey] = state
	}
	return state, nil
}

// writeTrustRecords splices the changed records' tables into raw and writes it, once the
// result reads back as doc: nothing outside those tables may change.
func writeTrustRecords(configPath string, raw []byte, doc, state map[string]any, keys []string) error {
	slices.Sort(keys)
	out, err := spliceTrustTables(raw, state, keys)
	if err != nil {
		return fmt.Errorf("rewrite %s: %w", configPath, err)
	}
	got := map[string]any{}
	if err := toml.Unmarshal(out, &got); err != nil {
		return fmt.Errorf("rewrite %s: the result does not parse (%w); nothing was written", configPath, err)
	}
	// An emptied [hooks.state] reads back absent; compare without the tables made for it.
	prune(doc)
	prune(got)
	want, err := renderTOMLValue(doc)
	if err != nil {
		return err
	}
	have, err := renderTOMLValue(got)
	if err != nil {
		return err
	}
	if want != have {
		return fmt.Errorf("rewrite %s: the result would change more than Terma's hook approvals; nothing was written", configPath)
	}
	return writeCodexConfig(configPath, out)
}

// prune drops the empty hooks tables a splice leaves no text for.
func prune(doc map[string]any) {
	hooks, _ := doc[codexHooksStateTable].(map[string]any)
	if hooks == nil {
		return
	}
	if state, ok := hooks[codexHooksStateKey].(map[string]any); ok && len(state) == 0 {
		delete(hooks, codexHooksStateKey)
	}
	if len(hooks) == 0 {
		delete(doc, codexHooksStateTable)
	}
}

// spliceTrustTables drops each keyed `[hooks.state."<key>"]` table's text and appends the
// ones still in state rendered again, so the rest of the file stays byte for byte.
func spliceTrustTables(raw []byte, state map[string]any, keys []string) ([]byte, error) {
	text := string(raw)
	eol := "\n"
	if strings.Contains(text, "\r\n") {
		eol = "\r\n"
	}
	header := func(key string) string {
		return "[" + codexHooksStateTable + "." + codexHooksStateKey + "." + quoteTOMLString(key) + "]"
	}
	drop := map[string]bool{}
	for _, k := range keys {
		drop[header(k)] = true
	}
	var lines []string
	if text != "" {
		lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	}
	clean := func(line string) string { return strings.TrimRight(line, "\r") }
	var out []string
	for i := 0; i < len(lines); {
		if !drop[strings.TrimSpace(clean(lines[i]))] {
			out = append(out, clean(lines[i]))
			i++
			continue
		}
		// A dropped table's blank separator goes with it.
		for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			out = out[:len(out)-1]
		}
		for i++; i < len(lines) && !anyHeaderRE.MatchString(clean(lines[i])); i++ {
		}
		if i < len(lines) && len(out) > 0 {
			out = append(out, "")
		}
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	for _, k := range keys {
		record, ok := state[k].(map[string]any)
		if !ok {
			continue
		}
		fields := make([]string, 0, len(record))
		for f := range record {
			fields = append(fields, f)
		}
		slices.Sort(fields)
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, header(k))
		for _, f := range fields {
			v, err := renderTOMLValue(record[f])
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", k, f, err)
			}
			out = append(out, quoteTOMLKey(f)+" = "+v)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return []byte(strings.Join(out, eol) + eol), nil
}

// codexTrustStatePath is the record under terma's config directory dir.
func codexTrustStatePath(dir string) string {
	return filepath.Join(dir, config.SetupDir, codexTrustState)
}

func loadCodexTrustRecord(path string) (*codexTrustRecord, error) {
	rec := &codexTrustRecord{}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return rec, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, rec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return rec, nil
}

func (rec *codexTrustRecord) save(path string) error {
	if len(rec.Configs) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return config.WriteJSON(path, rec, harness.SettingsMode)
}
