// Package routing holds what this machine knows about routing a repository's agents to
// its Terma project: one record per project (routing/<id>.json under the config
// directory) naming the developer's agents for it, its signals and its content policy.
// The local relay reads it to decide what of a claimed session may leave; install writes
// it; nothing in it is a secret.
//
// It was the half of the shim package that was not a shim: terma no longer wraps the
// agents' binaries, so this is all of per-repository routing that remains.
package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// The agents a record names, by the names the keystore and the records use.
const (
	// AgentClaude is Claude Code.
	AgentClaude = "claude"
	// AgentCodex is Codex (CLI and Desktop).
	AgentCodex = "codex"
)

// Record is a project's routing configuration on this machine, written by `terma
// install` under config.Dir(): which of the developer's agents report to the project,
// with which signals, and what content may leave — the policy the local relay enforces
// for the project's sessions (relay.Policy). It holds no secret: the key stays in the
// keystore, keyed by harness and project, so revoking one project's key never exposes
// another's.
type Record struct {
	ProjectID          string            `json:"project_id"`
	Endpoint           string            `json:"endpoint"`
	Signals            []string          `json:"signals"`
	CLI                bool              `json:"cli"`
	Desktop            bool              `json:"desktop"`
	IncludePrompts     bool              `json:"include_prompts"`
	IncludeToolContent bool              `json:"include_tool_content"`
	ResourceAttributes map[string]string `json:"resource_attributes,omitempty"`
	// Harnesses names the agents routed to this project for this developer.
	Harnesses []string `json:"harnesses"`
}

func dir(parts ...string) (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{base}, parts...)...), nil
}

// RoutingDir holds one <projectID>.json per routed project.
func RoutingDir() (string, error) { return dir("routing") }

func recordPath(projectID string) (string, error) {
	if !termaproject.ValidID(projectID) {
		return "", fmt.Errorf("unsafe project id %q", projectID)
	}
	d, err := RoutingDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, projectID+".json"), nil
}

// SaveRecord writes (or replaces) a project's routing record.
func SaveRecord(rec Record) error {
	p, err := recordPath(rec.ProjectID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(p, append(data, '\n'), 0o600)
}

// LoadRecord reads a project's routing record. ok is false when none exists.
func LoadRecord(projectID string) (rec Record, ok bool, err error) {
	p, err := recordPath(projectID)
	if err != nil {
		return Record{}, false, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

// MigrateCodexCLIRoutes gives every routing record written before the cli field (0.0.2
// and earlier) the meaning it had then: a record that routes codex routes the Codex CLI,
// and nothing routes Codex Desktop. Without it the router reads the missing field as
// false and silently stops exporting that developer's Codex CLI sessions. A record that
// already says either way is left alone, as is every field this build does not know; a
// record that does not parse is not this migration's to repair. It stops between records
// when ctx is done; the records it has not reached are migrated on a later start.
func MigrateCodexCLIRoutes(ctx context.Context) error {
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
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
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
