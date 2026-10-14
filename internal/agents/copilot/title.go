package copilot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// sourceCopilotWorkspace is where a session's name comes from: the runtime's workspace.yaml.
const sourceCopilotWorkspace = "copilot_workspace"

// workspaceMaxBytes bounds the read of a session's workspace.yaml, which holds its whole first
// prompt and is read at every turn's end: a larger one is not read.
const workspaceMaxBytes = 1 << 20

var copilotTitleStateDir = hookrun.AgentStateDir(name, "titles")

// captureTitle spools the session's name when new or renamed. The runtime names a session
// after its first prompt until someone renames it, so the name travels under titleConsented
// and, at delivery, the policy's prompt switch. Its telemetry carries no name.
func captureTitle(e hookrun.Env, r *hookrun.Repo, sessionID string) {
	if !titleConsented(e.Consent()) {
		return
	}
	attrs := map[string]any{semconv.GenAIMainAgentNameKey: name, semconv.TermaEvidenceSourceKey: sourceCopilotWorkspace}
	hookrun.SpoolTitle(e, r, copilotTitleStateDir, sessionID, attrs, func() (string, bool, error) {
		return readSessionName(sessionID)
	})
}

// readSessionName is the name the runtime keeps for sessionID in
// session-state/<id>/workspace.yaml: its first prompt, or the name someone gave it. found is
// false before the runtime has written one, and for a file that names another session.
func readSessionName(sessionID string) (title string, found bool, err error) {
	home, err := copilotHome()
	if err != nil {
		return "", false, err
	}
	path := filepath.Join(home, "session-state", sessionID, "workspace.yaml")
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.Size() > workspaceMaxBytes {
		return "", false, fmt.Errorf("%s is %d bytes, over the %d read", path, info.Size(), workspaceMaxBytes)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	var w struct {
		ID   string `yaml:"id"`
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(b, &w); err != nil {
		return "", false, err
	}
	if w.ID != "" && w.ID != sessionID {
		return "", false, nil
	}
	title = strings.TrimSpace(w.Name)
	return title, title != "", nil
}

// titleConsented reports whether a title may be sent at all: Copilot is among the developer's
// agents, which setup points at the relay, or global mode collects every session. The team's
// policy, checked at delivery, decides whether prompts may.
func titleConsented(c hookrun.Consent) bool {
	return c.Global || slices.Contains(c.Agents, name)
}
