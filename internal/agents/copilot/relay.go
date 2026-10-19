package copilot

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/shellrc"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// ConfigureRelay sends Copilot's own exporters to the local relay: the one VS Code's agent
// host runs, from VS Code's user settings, and the CLI's, which reads only its
// environment, through a shell function that starts it with `terma launch`. A settings or
// startup file terma cannot write is said and left: neither stops setup, nor the other.
// With neither pointed, the result is pending, so setup does not report the agent as exporting.
func (a Agent) ConfigureRelay(_ context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	var out agents.RelayResult
	a.configureVSCode(cfg, &out)
	configureCLI(&out)
	if len(out.Paths) == 0 {
		out.Pending = true
		if len(out.Notes) == 0 {
			out.Notes = append(out.Notes, "Found neither the "+command+" command nor a VS Code that Copilot has run in, so there is nothing of Copilot's to point at the relay yet. Run `terma setup` again once one is installed.")
		}
	}
	return out, nil
}

func (a Agent) configureVSCode(cfg agents.RelayConfig, out *agents.RelayResult) {
	dirs := vscodeUserDirs(false)
	if variable, set := shellEndpoint(strings.TrimPrefix(cfg.Endpoint, "http://")); set {
		// What an earlier setup wrote goes first, wherever a record says it is: its content
		// switch would now serve that endpoint.
		took := false
		for _, path := range a.undoPaths() {
			changed, _, err := a.disconnectVSCode(path, cfg.Token)
			if err != nil {
				out.Notes = append(out.Notes, err.Error())
			}
			took = took || changed
		}
		if len(dirs) == 0 && !took {
			return
		}
		note := variable + " is exported in your shell, and VS Code reads it ahead of its settings: Copilot there exports where it says, not to the relay, and terma's settings would add what was said to that export. Terma wrote none of its settings into VS Code"
		if took {
			note += ", and took out the ones an earlier setup wrote; restart VS Code so it stops using them"
		}
		out.Notes = append(out.Notes, note+". Unset "+variable+" where VS Code starts, then run `terma setup` again.")
		return
	}
	if len(dirs) == 0 {
		return
	}
	restart := false
	for _, dir := range dirs {
		path, changed, note, err := a.connectVSCode(dir, cfg)
		if err != nil {
			out.Notes = append(out.Notes, fmt.Sprintf("Left %s alone, so Copilot in VS Code exports nothing: %v.", output.TildePath(path), err))
			continue
		}
		out.Paths = append(out.Paths, path)
		restart = restart || changed
		if note != "" {
			out.Notes = append(out.Notes, note)
		}
	}
	if restart {
		out.Notes = append(out.Notes, "Restart VS Code so Copilot starts exporting there: it reads these settings as it starts.")
	}
}

func configureCLI(out *agents.RelayResult) {
	// A CLI that is not installed has nothing to start; setup run again after it is writes this.
	if _, err := exec.LookPath(command); err != nil {
		return
	}
	quiet := "the " + command + " command exports nothing when you start it yourself"
	if shellrc.LaunchersOff() {
		if _, err := shellrc.RemoveLauncher(command); err != nil {
			out.Notes = append(out.Notes, err.Error())
		}
		out.Notes = append(out.Notes, "TERMA_SHELL_LAUNCHERS=0 keeps terma's "+command+" function out of your shell: "+quiet+".")
		return
	}
	changed, err := shellrc.InstallLauncher(command)
	switch {
	case errors.Is(err, shellrc.ErrNoShell):
		out.Notes = append(out.Notes, "Terma defines the function that starts "+command+" with its exporter for zsh, bash and fish only: "+quiet+".")
		return
	case err != nil:
		out.Notes = append(out.Notes, fmt.Sprintf("Could not write the %s function into your shell's startup file (%v): %s.", command, err, quiet))
		return
	}
	if rc, ok := shellrc.ShellRC(); ok {
		out.Paths = append(out.Paths, rc.Paths...)
	}
	if len(changed) > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("Open a new terminal, or run `source %s`, so `%s` starts with its exporter.", output.TildePath(changed[0]), command))
	}
}

// UnconfigureRelay restores VS Code's settings to what they held before setup, wherever a
// record says setup wrote them, and takes the CLI's function out of the shell startup
// files, each whatever becomes of the other.
func (a Agent) UnconfigureRelay(_ context.Context, token string) (agents.RelayResult, error) {
	var out agents.RelayResult
	var errs []error
	for _, path := range a.undoPaths() {
		changed, skipped, err := a.disconnectVSCode(path, token)
		if err != nil {
			errs = append(errs, err)
			continue
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
	return out, errors.Join(append(errs, err)...)
}

// RelayPointed reports whether the exporter of VS Code's agent host sends to the relay at
// addr. It is unknown on a machine with no VS Code Copilot runs in, where only the CLI's
// environment could say, and where an endpoint in the shell keeps setup from writing the
// settings: RelayProblem says that.
// Accepted risk: an organization's Copilot telemetry policy outranks the settings and is not
// read, so under one that manages the exporter this reports pointed while the relay gets
// nothing. Upgrade path: read the managed configuration once its shape is known (README).
func (Agent) RelayPointed(addr string) (pointed, known bool) {
	dirs := vscodeUserDirs(false)
	for _, dir := range dirs {
		if !vscodePointed(filepath.Join(dir, "settings.json"), addr) {
			_, blocked := shellEndpoint(addr)
			return false, !blocked
		}
	}
	return len(dirs) > 0, len(dirs) > 0
}

// RelayProblem names what still keeps Copilot's telemetry from the relay at addr after setup.
func (Agent) RelayProblem(addr string) (detail, fix string, ok bool) {
	if variable, set := shellEndpoint(addr); set && len(vscodeUserDirs(false)) > 0 {
		return variable + " is exported in your shell, and VS Code reads it ahead of its settings, so Copilot in VS Code exports where it says and not to the relay",
			"unset " + variable + " where VS Code starts, then run `terma setup`", true
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
// are cleared for this one process. The CLI keeps all of these out of what it runs.
func (Agent) LaunchEnv(cfg agents.RelayConfig) (set map[string]string, unset []string) {
	return map[string]string{
		"COPILOT_OTEL_ENABLED":                               "true",
		harness.EnvOTLPEndpoint:                              cfg.Endpoint + "/" + cfg.Token,
		harness.EnvOTLPProtocol:                              "http/json",
		"OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT": "true",
	}, []string{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
		"COPILOT_OTEL_ENDPOINT", "COPILOT_OTEL_EXPORTER_TYPE", "COPILOT_OTEL_FILE_EXPORTER_PATH",
	}
}

var (
	_ agents.RelayExporter = Agent{}
	_ agents.RelayChecker  = Agent{}
	_ agents.Launched      = Agent{}
)
