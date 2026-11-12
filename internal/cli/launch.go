package cli

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/shellrc"
)

// launchCommand reports whether args run `terma launch`, which must start the agent's
// command whatever else fails.
func launchCommand(args []string) bool {
	return len(args) > 0 && args[0] == "launch"
}

func (app *App) newLaunchCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "launch <agent> [-- args...]",
		Short:  "Internal: start an agent's command with its relay exporter in the environment",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ok := app.agents.Find[agents.Launched](args[0])
			if !ok {
				return fmt.Errorf("terma launch: %q is not a command terma starts", args[0])
			}
			name := a.LaunchCommand()
			path, err := exec.LookPath(name)
			if err != nil {
				// As the shell says it, with the shell's status.
				fmt.Fprintf(cmd.ErrOrStderr(), "%s: command not found\n", name)
				return exitWith(127)
			}
			env := os.Environ()
			if cfg, ok := app.launchRelay(a); ok {
				set, unset := a.LaunchEnv(cfg)
				env = withEnv(env, set, unset)
			}
			// The separator the shell function writes is terma's; the rest is the command's.
			rest := args[1:]
			if len(rest) > 0 && rest[0] == "--" {
				rest = rest[1:]
			}
			return execCommand(path, append([]string{name}, rest...), env)
		},
	}
	// Everything after the agent's name is the command's own, flags included.
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// launchRelay is where the relay listens and its token, read without creating anything,
// for an agent setup last recorded as chosen: a shell still holding the function after
// setup dropped the agent, or after a teardown, starts the command bare. So does one
// under the kill switch.
func (app *App) launchRelay(a agents.Agent) (agents.RelayConfig, bool) {
	if app.stateDir == "" || shellrc.LaunchersOff() {
		return agents.RelayConfig{}, false
	}
	// One small local read, as a hook's.
	cfg, err := config.Load(app.dir, app.stateDir, config.Overrides{})
	if err != nil || !slices.ContainsFunc(agents.Selections(a), func(s string) bool { return slices.Contains(cfg.Harnesses, s) }) {
		return agents.RelayConfig{}, false
	}
	token, err := daemon.Token(app.stateDir)
	if err != nil || token == "" {
		return agents.RelayConfig{}, false
	}
	return agents.RelayConfig{Endpoint: "http://" + daemon.Addr(claim.Dir(app.stateDir)), Token: token}, true
}

// withEnv is env with set's variables in place of any it held and unset's gone.
func withEnv(env []string, set map[string]string, unset []string) []string {
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		key, _, _ := strings.Cut(kv, "=")
		_, replaced := set[key]
		return replaced || slices.Contains(unset, key)
	})
	for key, value := range set {
		out = append(out, key+"="+value)
	}
	return out
}
