package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/style"
)

// newRelayCommand is the loopback relay every agent's global export points at
// (docs/RELAY.md). `serve` is what launchd or systemd runs; `status` is for a person.
// `terma setup` installs and starts the service.
func newRelayCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "relay",
		Short:  "The local relay agents export telemetry through (internal)",
		Hidden: true,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "serve",
		Short: "Run the relay in the foreground (the service manager runs this)",
		Args:  cobra.NoArgs,
		RunE:  runRelayServe,
	}, &cobra.Command{
		Use:   "status",
		Short: "Show whether the relay is running and delivering",
		Args:  cobra.NoArgs,
		RunE:  runRelayStatus,
	})
	return cmd
}

func runRelayServe(cmd *cobra.Command, _ []string) error {
	rc, err := relay.Load()
	if errors.Is(err, relay.ErrNotConfigured) {
		return errors.New("the relay is not set up on this machine; run `terma setup`")
	}
	if err != nil {
		return err
	}
	dir, err := relay.Dir()
	if err != nil {
		return err
	}
	log := relay.NewLog(dir)
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = relay.Serve(ctx, relay.Options{
		Config:         rc,
		Dir:            dir,
		Version:        Version,
		Logf:           log.Printf,
		Binding:        relayBinding,
		Destination:    relayDestination,
		MachineProject: relayMachineProject,
	})
	if err != nil {
		log.Printf("relay stopped: %v", err)
	}
	return err
}

// relayBinding names the project a session's directory is bound to — its checkout's own
// binding, else its main checkout's in a linked worktree.
func relayBinding(dir string) (string, error) {
	f, _, err := termaproject.ResolveDir(dir)
	if errors.Is(err, termaproject.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return f.Project.ID, nil
}

// relayDestination is where a project's records go: the project's key from the keystore,
// to its key's own environment (projectEndpoint, as the spool does). Read afresh for every
// delivery, so a key `terma install` stores reaches a running relay.
func relayDestination(project string) (relay.Destination, error) {
	key := relayKey(project)
	if key == "" {
		return relay.Destination{}, relay.ErrHeld
	}
	cfg, err := loadConfig()
	if err != nil {
		return relay.Destination{}, err
	}
	return relay.Destination{Endpoint: projectEndpoint(cfg, project), Authorization: "Bearer " + key}, nil
}

// relayKey is a key that can deliver project's records: its spool key, else the key an
// agent was connected with.
func relayKey(project string) string {
	if key := keystore.Get(project); key != "" {
		return key
	}
	for _, h := range []string{"claude", "codex", "opencode"} {
		if key := keystore.GetFor(h, project); key != "" {
			return key
		}
	}
	return ""
}

// relayMachineProject names the machine project the relay sends everything else to.
var relayMachineProject = func() string {
	cfg, err := loadConfig()
	if err != nil {
		return ""
	}
	return machineProjectID(cfg)
}

func runRelayStatus(cmd *cobra.Command, _ []string) error {
	out := style.Highlight(cmd.OutOrStdout())
	rc, err := relay.Load()
	if errors.Is(err, relay.ErrNotConfigured) {
		fmt.Fprintln(out, "Relay:    not set up — agents export straight to Terma. `terma setup` sets it up.")
		return nil
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
	defer cancel()
	svc, err := relay.Service(ctx)
	if err != nil {
		return err
	}
	switch {
	case !svc.Installed:
		fmt.Fprintln(out, "Service:  not installed — `terma setup` installs it")
	case svc.Running:
		fmt.Fprintf(out, "Service:  running (%s)\n", tildePath(svc.Binary))
	default:
		fmt.Fprintf(out, "Service:  installed, not running (%s)\n", tildePath(svc.Binary))
	}
	fmt.Fprintf(out, "Endpoint: %s\n", rc.Endpoint())
	health, err := relay.Probe(ctx, rc)
	if err != nil {
		fmt.Fprintf(out, "Health:   not answering (%v)\n", err)
		return nil
	}
	fmt.Fprintf(out, "Health:   answering, terma %s\n", health.Version)
	fmt.Fprintf(out, "Backlog:  %d waiting to be delivered\n", health.Backlog)
	c := health.Counters
	fmt.Fprintf(out, "Since start: %d received, %d routed, %d delivered, %d refused, %d dropped\n",
		c.Received, c.Routed, c.Delivered, c.Dead, c.Dropped)
	var held []string
	for _, p := range health.HeldProjects {
		if p == relay.MachineRoute {
			fmt.Fprintln(out, "Held:     records for the machine project — none is chosen yet; `terma setup` chooses one")
			continue
		}
		held = append(held, p)
	}
	if len(held) > 0 {
		fmt.Fprintf(out, "Held:     %s — no key on this machine; `terma install` in a repository bound to it stores one\n",
			strings.Join(held, ", "))
	}
	if health.LastError != "" {
		fmt.Fprintf(out, "Last error: %s\n", health.LastError)
	}
	if dir, err := relay.Dir(); err == nil {
		fmt.Fprintf(out, "Log:      %s\n", tildePath(relay.NewLog(dir).Path()))
	}
	return nil
}

// machineProjectID is the project chosen for this machine in `terma setup`.
func machineProjectID(cfg *config.Config) string { return cfg.Telemetry.Project.ID }
