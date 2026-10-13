package copilot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/shellrc"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// ConfigureRelay sends both of Copilot's own exporters to the local relay: VS Code's from
// its user settings, and the CLI's, which reads only its environment, through a shell
// function that starts it with `terma launch`.
func (a Agent) ConfigureRelay(_ context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	var out agents.RelayResult
	restart := false
	for _, dir := range vscodeUserDirs() {
		path, changed, err := a.connectVSCode(dir, cfg)
		if err != nil {
			return out, err
		}
		out.Paths = append(out.Paths, path)
		restart = restart || changed
	}
	if restart {
		out.Notes = append(out.Notes, "Restart VS Code so Copilot starts exporting there: it reads these settings as it starts.")
	}
	// A CLI that is not installed has nothing to start; setup run again after it is writes this.
	if _, err := exec.LookPath(command); err != nil {
		return out, nil
	}
	if shellrc.LaunchersOff() {
		_, err := shellrc.RemoveLauncher(command)
		return out, err
	}
	changed, err := shellrc.InstallLauncher(command)
	switch {
	case errors.Is(err, shellrc.ErrNoShell):
		out.Notes = append(out.Notes, "The "+command+" command exports nothing when you start it yourself: terma defines the function that starts it with its exporter for zsh, bash and fish only.")
	case err != nil:
		return out, err
	}
	if rc, ok := shellrc.ShellRC(); ok {
		out.Paths = append(out.Paths, rc.Paths...)
	}
	if len(changed) > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("Open a new terminal, or run `source %s`, so `%s` starts with its exporter.", output.TildePath(changed[0]), command))
	}
	return out, nil
}

// UnconfigureRelay restores VS Code's settings to what they held before setup and takes
// the CLI's function out of the shell startup files.
func (a Agent) UnconfigureRelay(_ context.Context, token string) (agents.RelayResult, error) {
	var out agents.RelayResult
	for _, dir := range vscodeUserDirs() {
		path := filepath.Join(dir, "settings.json")
		changed, skipped, err := a.disconnectVSCode(path, token)
		if err != nil {
			return out, err
		}
		if changed {
			out.Paths = append(out.Paths, path)
		}
		for _, key := range skipped {
			out.Notes = append(out.Notes, fmt.Sprintf("Did not restore %s in %s: it was changed after setup. Check it holds what you want.", key, output.TildePath(path)))
		}
	}
	removed, err := shellrc.RemoveLauncher(command)
	out.Paths = append(out.Paths, removed...)
	return out, err
}

// RelayPointed reports whether VS Code's Copilot exporter sends to the relay at addr; it
// is unknown on a machine with no VS Code, where only the CLI's environment could say.
func (Agent) RelayPointed(addr string) (pointed, known bool) {
	dirs := vscodeUserDirs()
	for _, dir := range dirs {
		if !vscodePointed(filepath.Join(dir, "settings.json"), addr) {
			return false, true
		}
	}
	return len(dirs) > 0, len(dirs) > 0
}

// RelayProblem names what still keeps Copilot's telemetry from the relay after setup.
func (Agent) RelayProblem() (detail, fix string, ok bool) {
	for _, dir := range vscodeUserDirs() {
		path := filepath.Join(dir, "settings.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if doc, err := parseSettings(data); err == nil {
			if level, _ := doc.text(telemetryLevel); level == "off" {
				return "VS Code's " + telemetryLevel + " is off in " + output.TildePath(path) + ", which also switches off Copilot's OpenTelemetry export there",
					"set " + telemetryLevel + " to error, crash or all, then restart VS Code", true
			}
		}
	}
	if _, err := exec.LookPath(command); err == nil && !shellrc.LauncherInstalled(command) {
		quiet := ", so the Copilot CLI exports nothing when you start it yourself"
		if shellrc.LaunchersOff() {
			return "TERMA_SHELL_LAUNCHERS=0 keeps terma's " + command + " function out of your shell" + quiet,
				"unset TERMA_SHELL_LAUNCHERS and run `terma setup`", true
		}
		if _, ok := shellrc.ShellRC(); !ok {
			return "terma defines its " + command + " function for zsh, bash and fish only" + quiet,
				"start " + command + " from zsh, bash or fish, where `terma setup` defines the function", true
		}
		return "your shell does not start " + command + " through terma" + quiet, "terma setup", true
	}
	return "", "", false
}

// LaunchCommand is the Copilot CLI.
func (Agent) LaunchCommand() string { return command }

// LaunchEnv is the CLI's exporter, pointed at the relay with all content: the relay
// withholds what the team's policy does not collect. The token rides the endpoint path, as
// in VS Code's settings: a header would also go to a per-signal endpoint of the
// developer's own. Those, and a file exporter, would take the export elsewhere, so they
// are cleared for this one process. The CLI keeps OTEL_EXPORTER_* out of what it runs.
func (Agent) LaunchEnv(cfg agents.RelayConfig) (set map[string]string, unset []string) {
	return map[string]string{
		"COPILOT_OTEL_ENABLED":                               "true",
		harness.EnvOTLPEndpoint:                              cfg.Endpoint + "/" + cfg.Token,
		harness.EnvOTLPProtocol:                              "http/json",
		"OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT": "true",
	}, []string{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
		"COPILOT_OTEL_EXPORTER_TYPE", "COPILOT_OTEL_FILE_EXPORTER_PATH",
	}
}

var (
	_ agents.RelayExporter = Agent{}
	_ agents.RelayChecker  = Agent{}
	_ agents.Launched      = Agent{}
)
