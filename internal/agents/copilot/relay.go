package copilot

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/shellrc"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// ConfigureRelay sends Copilot's own exporters to the local relay: the one VS Code's agent
// host runs, from VS Code's user settings, and the CLI's, which reads only its
// environment, through a shell function that starts it with `terma launch`. A settings or
// startup file terma cannot write is said and left: neither stops setup, nor the other.
func (a Agent) ConfigureRelay(_ context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	var out agents.RelayResult
	a.configureVSCode(cfg, &out)
	configureCLI(&out)
	return out, nil
}

func (a Agent) configureVSCode(cfg agents.RelayConfig, out *agents.RelayResult) {
	dirs := vscodeUserDirs()
	if len(dirs) == 0 {
		return
	}
	if variable, set := shellEndpoint(); set {
		// What an earlier setup wrote goes too: its content switch would now serve that endpoint.
		for _, dir := range dirs {
			if _, _, err := a.disconnectVSCode(filepath.Join(dir, "settings.json"), cfg.Token); err != nil {
				out.Notes = append(out.Notes, err.Error())
			}
		}
		out.Notes = append(out.Notes, variable+" is exported in your shell, and VS Code reads it ahead of its settings: Copilot there would export to it, content included, and not to the relay. Terma left VS Code's settings alone; unset it where VS Code starts, then run `terma setup` again.")
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

// UnconfigureRelay restores VS Code's settings to what they held before setup and takes
// the CLI's function out of the shell startup files, each whatever becomes of the other.
func (a Agent) UnconfigureRelay(_ context.Context, token string) (agents.RelayResult, error) {
	var out agents.RelayResult
	var errs []error
	for _, dir := range vscodeUserDirs() {
		path := filepath.Join(dir, "settings.json")
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
func (Agent) RelayPointed(addr string) (pointed, known bool) {
	dirs := vscodeUserDirs()
	for _, dir := range dirs {
		if !vscodePointed(filepath.Join(dir, "settings.json"), addr) {
			_, blocked := shellEndpoint()
			return false, !blocked
		}
	}
	return len(dirs) > 0, len(dirs) > 0
}

// RelayProblem names what still keeps Copilot's telemetry from the relay after setup.
func (Agent) RelayProblem() (detail, fix string, ok bool) {
	if variable, set := shellEndpoint(); set && len(vscodeUserDirs()) > 0 {
		return variable + " is exported in your shell, and VS Code reads it ahead of its settings, so Copilot in VS Code exports nothing to the relay",
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
