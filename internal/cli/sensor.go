package cli

import (
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/sensor/sensorctl"
)

// newSensorCommand is the opt-in sensor: it follows agent sessions' processes, network and
// files on this machine, as root, and answers a local query API. Hidden while it is a
// spike: nothing installs or starts it.
func (app *App) newSensorCommand() *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:    "sensor",
		Short:  "Follow agent sessions' processes, network and files (opt-in, needs root)",
		Hidden: true,
	}
	cmd.PersistentFlags().StringVar(&socket, "socket", sensorctl.DefaultSocket(), "the query API's unix socket")

	var policy, docker, relay, tokenFile string
	var extra []string
	run := &cobra.Command{
		Use:   "run",
		Short: "Run the sensor until interrupted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			var token []byte
			if tokenFile != "" {
				var err error
				if token, err = os.ReadFile(tokenFile); err != nil {
					return err
				}
			}
			return sensorctl.Run(ctx, sensorctl.Options{
				Socket: socket, Roots: append(app.agents.ProcessNames(), extra...),
				PolicyFile: policy, Docker: docker, Relay: relay, RelayToken: strings.TrimSpace(string(token)),
				Log: slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil)),
			})
		},
	}
	run.Flags().StringVar(&policy, "policy", "", "a policy file to apply at start")
	run.Flags().StringVar(&docker, "docker", "/var/run/docker.sock", "the container engine's socket; empty skips containers")
	run.Flags().StringVar(&relay, "relay", "", "the relay's URL to send rollups to; empty keeps them local")
	run.Flags().StringVar(&tokenFile, "relay-token-file", "", "the file holding the relay's token")
	run.Flags().StringSliceVar(&extra, "watch-exe", nil, "more process names that start a session, beside the agents'")

	query := func(use, short, path string, args cobra.PositionalArgs) *cobra.Command {
		return &cobra.Command{
			Use: use, Short: short, Args: args,
			RunE: func(cmd *cobra.Command, args []string) error {
				p := path
				if len(args) == 1 {
					p += "/" + args[0]
				}
				return sensorctl.Get(socket, p, cmd.OutOrStdout())
			},
		}
	}
	change := func(use, short, method, path string, body bool) *cobra.Command {
		return &cobra.Command{
			Use: use, Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if !body {
					return sensorctl.Send(socket, method, path+"/"+args[0], nil)
				}
				f, err := os.Open(args[0])
				if err != nil {
					return err
				}
				defer f.Close()
				return sensorctl.Send(socket, method, path, f)
			},
		}
	}
	cmd.AddCommand(
		run,
		&cobra.Command{
			Use: "top", Short: "Show the sessions live", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error { return sensorctl.Top(socket, cmd.OutOrStdout()) },
		},
		query("sessions", "Print every session's totals as JSON", "/v1/sessions", cobra.NoArgs),
		query("session <root>", "Print one session in full as JSON", "/v1/sessions", cobra.ExactArgs(1)),
		change("watch <pid>", "Watch a running process as a session's root", http.MethodPost, "/v1/watch", false),
		change("policy <file>", "Apply a policy file", http.MethodPut, "/v1/policy", true),
	)
	return cmd
}
