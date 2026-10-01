// Package routing keeps one secret-free record per project under the config directory:
// the developer's agents for it, its signals and its content policy, which install
// writes and the local relay enforces.
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

// Record is a project's routing configuration on this machine; its key stays in the keystore.
type Record struct {
	ProjectID          string            `json:"project_id"`
	Endpoint           string            `json:"endpoint"`
	Signals            []string          `json:"signals"`
	IncludePrompts     bool              `json:"include_prompts"`
	IncludeToolContent bool              `json:"include_tool_content"`
	ResourceAttributes map[string]string `json:"resource_attributes,omitempty"`
	Harnesses          []string          `json:"harnesses"`
	// Surfaces names the agents.Surface values routed here; an agent with one surface is named by it.
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
