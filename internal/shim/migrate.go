package shim

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// MigrateCodexCLIRoutes gives every routing record written before the cli field (0.0.2
// and earlier) the meaning it had then: a record that routes codex routes the Codex CLI,
// and nothing routes Codex Desktop. Without it the router reads the missing field as
// false and silently stops exporting that developer's Codex CLI sessions. A record that
// already says either way is left alone, as is every field this build does not know; a
// record that does not parse is not this migration's to repair.
func MigrateCodexCLIRoutes() error {
	dir, err := RoutingDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		errs = append(errs, addCLIField(filepath.Join(dir, e.Name())))
	}
	return errors.Join(errs...)
}

func addCLIField(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var rec map[string]json.RawMessage
	if json.Unmarshal(data, &rec) != nil || rec == nil {
		return nil
	}
	if _, ok := rec["cli"]; ok {
		return nil
	}
	var harnesses []string
	_ = json.Unmarshal(rec["harnesses"], &harnesses)
	rec["cli"] = json.RawMessage(strconv.FormatBool(slices.Contains(harnesses, AgentCodex)))
	if _, ok := rec["desktop"]; !ok {
		rec["desktop"] = json.RawMessage("false")
	}
	out, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, append(out, '\n'), info.Mode().Perm())
}
