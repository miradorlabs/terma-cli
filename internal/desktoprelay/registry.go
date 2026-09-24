// Package desktoprelay routes Codex desktop OTLP logs through the repository whose
// trusted SessionStart hook announced the conversation.
package desktoprelay

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/session"
)

const sessionTTL = 14 * 24 * time.Hour

type registration struct {
	ProjectID string    `json:"project_id"`
	SeenAt    time.Time `json:"seen_at"`
	// Ambiguous is sticky: moving one conversation between projects must never
	// cause its later OTLP records to be sent to either project by guesswork.
	Ambiguous bool `json:"ambiguous,omitempty"`
}

type registryFile struct {
	Sessions map[string]registration `json:"sessions"`
}

func registryPath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "desktop-relay", "sessions.json"), nil
}

func readRegistry(path string) (registryFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return registryFile{Sessions: map[string]registration{}}, nil
	}
	if err != nil {
		return registryFile{}, err
	}
	var file registryFile
	if err := json.Unmarshal(data, &file); err != nil {
		return registryFile{}, err
	}
	if file.Sessions == nil {
		file.Sessions = map[string]registration{}
	}
	return file, nil
}

// Register records the project of a Codex conversation announced by a trusted
// repository hook. Hooks call this once at session start and never wait on network.
func Register(sessionID, projectID string, now time.Time) error {
	if !session.ValidID(sessionID) || !project.ValidID(projectID) {
		return errors.New("invalid desktop session or project id")
	}
	path, err := registryPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	unlock, err := flock.Lock(ctx, path+".lock")
	if err != nil {
		return err
	}
	defer unlock()
	file, err := readRegistry(path)
	if err != nil {
		return err
	}
	for id, rec := range file.Sessions {
		if now.Sub(rec.SeenAt) > sessionTTL {
			delete(file.Sessions, id)
		}
	}
	rec, exists := file.Sessions[sessionID]
	if exists && (rec.Ambiguous || rec.ProjectID != projectID) {
		rec.ProjectID = ""
		rec.Ambiguous = true
	} else {
		rec.ProjectID = projectID
	}
	rec.SeenAt = now.UTC()
	file.Sessions[sessionID] = rec
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	return config.WriteFileAtomicNoSync(path, data, 0o600)
}

// ProjectFor returns the project a trusted hook registered for a conversation.
// Missing, stale, or ambiguous registrations never resolve to a project.
func ProjectFor(sessionID string, now time.Time) string {
	if !session.ValidID(sessionID) {
		return ""
	}
	path, err := registryPath()
	if err != nil {
		return ""
	}
	file, err := readRegistry(path)
	if err != nil {
		return ""
	}
	rec := file.Sessions[sessionID]
	if rec.Ambiguous || rec.ProjectID == "" || now.Sub(rec.SeenAt) > sessionTTL {
		return ""
	}
	return rec.ProjectID
}
