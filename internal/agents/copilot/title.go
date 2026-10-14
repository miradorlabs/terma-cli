package copilot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// sourceCopilotWorkspace is where a session's name comes from: the runtime's workspace.yaml.
const sourceCopilotWorkspace = "copilot_workspace"

// copilotTitleMaxText bounds a title: the runtime names a session after its whole first prompt.
const copilotTitleMaxText = 256

var copilotTitleStateDir = hookrun.AgentStateDir(name, "titles")

// copilotTitleState keeps an unchanged name from being sent at every turn's end.
type copilotTitleState struct {
	Title string `json:"title"`
}

// captureTitle spools the session's name when new or renamed. The runtime names a session
// after its first prompt until someone renames it, so the name travels under titleConsented
// and, at delivery, the policy's prompt switch. Its telemetry carries no name.
func captureTitle(e hookrun.Env, r *hookrun.Repo, sessionID string) {
	if e.Spool == nil || !session.ValidID(sessionID) || !titleConsented(e.Consent()) {
		return
	}
	dir := filepath.Join(e.StateDir, copilotTitleStateDir)
	path := filepath.Join(dir, hookrun.EvidenceID(sessionID)+".json")
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var last copilotTitleState
	b, readErr := os.ReadFile(path)
	if readErr == nil && json.Unmarshal(b, &last) != nil {
		last = copilotTitleState{}
	}
	title, found, err := readSessionName(sessionID)
	if err != nil {
		e.Logf("copilot title: %v", err)
		return
	}
	title = truncateBytes(title, copilotTitleMaxText)
	if !found || title == last.Title {
		return
	}
	attrs := map[string]any{
		semconv.GenAIMainAgentNameKey: name, semconv.TermaEvidenceSourceKey: sourceCopilotWorkspace,
		semconv.TermaSessionTitleKey: title, hookrun.AttrProjectID: r.ProjectID,
	}
	if err := e.Spool.Append(spool.Event{Time: e.Time(), Name: semconv.TermaSessionTitleEvent, SessionID: sessionID, Repository: r.Repository, Global: e.Policy.Global(), Attrs: attrs}); err != nil {
		e.Logf("copilot title: %v", err)
		return
	}
	if b, err := json.Marshal(copilotTitleState{Title: title}); err == nil {
		if err := hookrun.WriteState(path, b); err != nil {
			e.Logf("title state: %v", err)
		}
	}
	if os.IsNotExist(readErr) {
		hookrun.PruneState(dir, e.Time().Add(-spool.MaxAge))
	}
}

// readSessionName is the name the runtime keeps for sessionID in
// session-state/<id>/workspace.yaml: its first prompt, or the name someone gave it. found is
// false before the runtime has written one, and for a file that names another session.
func readSessionName(sessionID string) (title string, found bool, err error) {
	home, err := copilotHome()
	if err != nil {
		return "", false, err
	}
	b, err := os.ReadFile(filepath.Join(home, "session-state", sessionID, "workspace.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
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

// truncateBytes cuts s to at most n bytes on a rune boundary.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
