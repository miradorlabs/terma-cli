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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/config"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
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
	IncludePrompts     bool              `json:"include_prompts"`
	IncludeToolContent bool              `json:"include_tool_content"`
	ResourceAttributes map[string]string `json:"resource_attributes,omitempty"`
	// Harnesses names the agents routed to this project for this developer.
	Harnesses []string `json:"harnesses"`
	// Surfaces names the ways the developer runs them that are routed here (an
	// agents.Surface: Codex's CLI and desktop app are two; an agent with one surface is
	// named by it).
	Surfaces []string `json:"surfaces,omitempty"`
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
