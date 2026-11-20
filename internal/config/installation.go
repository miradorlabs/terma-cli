package config

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

// installationIDFileName holds this installation's id: one plain value, so no extension.
const installationIDFileName = "installation-id"

var installationIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// InstallationID is this installation's stable id, a random UUID made the first time it
// is asked for and kept in the config directory dir. Every sign-in sends it, so the web
// app groups a machine's sessions. It identifies nothing but
// this config directory, and authorizes nothing.
func InstallationID(dir string) (string, error) {
	if id, ok := readInstallationID(dir); ok {
		return id, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// Two first runs at once must agree on one id.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(dir, installationIDFileName)
	unlock, err := flock.Lock(ctx, path+".lock")
	if err != nil {
		return "", fmt.Errorf("lock the installation id: %w", err)
	}
	defer unlock()
	if id, ok := readInstallationID(dir); ok {
		return id, nil
	}
	id, err := newUUID()
	if err != nil {
		return "", err
	}
	if err := WriteFileAtomic(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("store the installation id: %w", err)
	}
	return id, nil
}

// StoredInstallationID is the installation id when one was made, without making one.
func StoredInstallationID(dir string) string {
	id, _ := readInstallationID(dir)
	return id
}

func readInstallationID(dir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(dir, installationIDFileName))
	if err != nil {
		return "", false
	}
	id := strings.TrimSpace(string(data))
	return id, installationIDPattern.MatchString(id)
}

// newUUID is a random (version 4) UUID.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate the installation id: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
