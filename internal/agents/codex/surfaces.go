package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// desktop's threads run in codex app-server and reach Terma through the relay and the
// repository's trusted hooks.
const desktop = "codex-desktop"

// Surfaces are Codex's CLI and its desktop app, chosen apart at setup.
func (a Agent) Surfaces() []agents.Surface {
	return []agents.Surface{
		{Name: "codex", DisplayName: "Codex CLI", Installed: a.Installed},
		{
			Name: desktop, DisplayName: "Codex Desktop", Installed: desktopInstalled,
			Needs: agents.Needs{Signals: []string{"logs"}, Hooks: true},
			InstallSteps: []string{"Approve Codex Desktop capture:\n" +
				"a. Open this repository in Codex Desktop and trust the project if prompted.\n" +
				"b. Open Settings → Hooks, then select Review for the entries from .codex/hooks.json.\n" +
				"c. Inspect and approve each Terma hook command for full capture. Codex CLI is not required.\n" +
				"d. Run `terma agent status codex-desktop` to confirm 'Codex hooks: ready', then start a new Local task in this repository."},
			SetupSteps: []string{"Codex Desktop: in a connected repository, open Settings → Hooks → Review in Codex Desktop and approve Terma's hooks."},
			Reports:    "reports through the relay and this repository's hooks",
			Warn: func() string {
				if global, err := (Codex{}).Status(); err == nil && global.Connected {
					return "also has a user-level exporter; it may send Desktop activity from other repositories"
				}
				return ""
			},
		},
	}
}

// CheckedSurfaces is Codex Desktop: the CLI's readiness is its exporter's, which doctor
// judges for every harness.
func (Agent) CheckedSurfaces() []string { return []string{desktop} }

// CheckSurface is Codex Desktop's readiness at root: a desktop route with logs, a delivery
// key, and trusted hooks.
func (a Agent) CheckSurface(_, root, projectID string) (agents.SurfaceStatus, error) {
	route, recorded, err := routing.LoadRecord(projectID)
	if err != nil {
		return agents.SurfaceStatus{}, fmt.Errorf("read this repository's Codex Desktop route: %w", err)
	}
	var st agents.SurfaceStatus
	routed := recorded && slices.Contains(route.Surfaces, desktop) && slices.Contains(route.Harnesses, name) && slices.Contains(route.Signals, "logs")
	switch {
	case !routed:
		st.Problem, st.Fix = "this repository has no Codex Desktop hook route", "terma install --signals logs"
	case keystore.GetFor(name, projectID) == "" && keystore.Get(projectID) == "":
		st.Problem, st.Fix = "this repository has no delivery key", "terma install"
	default:
		st.Ready = true
	}
	global, err := (Codex{}).Status()
	if err != nil {
		return st, err
	}
	st.Lines = append(st.Lines, agents.StatusLine{Label: "Repository", Value: projectID},
		agents.StatusLine{Label: "Desktop route", Value: readiness(st.Ready)},
		agents.StatusLine{Label: "Global export", Value: map[bool]string{true: "on (may include other repositories)", false: "off"}[global.Connected]})
	if recorded {
		st.Lines = append(st.Lines, agents.StatusLine{Label: "Prompt text", Value: onOff(route.IncludePrompts)},
			agents.StatusLine{Label: "Tool content", Value: onOff(route.IncludeToolContent)})
	}
	trust, err := a.Trust(root)
	if err != nil {
		return st, err
	}
	st.Lines = append(st.Lines, agents.StatusLine{Label: "Codex hooks", Value: readiness(trust.Trusted)})
	if !trust.Trusted && trust.Fix != "" {
		st.Lines = append(st.Lines, agents.StatusLine{Label: "Next", Value: trust.Fix})
	}
	return st, nil
}

func readiness(ready bool) string {
	if ready {
		return "ready"
	}
	return "not ready"
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func desktopInstalled(context.Context) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	home, _ := os.UserHomeDir()
	for _, path := range []string{"/Applications/ChatGPT.app", filepath.Join(home, "Applications", "ChatGPT.app")} {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}
