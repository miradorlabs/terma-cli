package cmd

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
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
		Use:   "supervise",
		Short: "Run the relay and start it again whenever it exits (Windows' service)",
		Args:  cobra.NoArgs,
		RunE:  runRelaySupervise,
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
	ensureMachineContentPolicy(log.Printf)
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if exe, err := os.Executable(); err == nil {
		go relayUpdater(ctx, exe, log.Printf, stop)
	}
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

// runRelaySupervise is what the Windows launcher runs at logon: the relay under a
// supervisor, which starts it again after a crash or a self-update's exit. The binary is
// resolved once, before an update can rename the running one aside, so the path it
// starts is always the installed terma.
func runRelaySupervise(cmd *cobra.Command, _ []string) error {
	dir, err := relay.Dir()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := relay.RecordSupervisor(); err != nil {
		return err
	}
	log := relay.NewLog(dir)
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	relay.Supervise(ctx, func(ctx context.Context) *exec.Cmd {
		c := exec.CommandContext(ctx, exe, "relay", "serve")
		c.SysProcAttr = relay.HiddenProcess()
		return c
	}, log.Printf)
	return nil
}

// ensureMachineContentPolicy records the machine's content default from the choices
// setup saved on the profile when the relay has none — a relay set up before policies
// existed would otherwise withhold every record's content, the fail-closed default.
func ensureMachineContentPolicy(logf func(string, ...any)) {
	if _, ok := relay.LoadContentPolicy(relay.MachineRoute); ok {
		return
	}
	cfg, err := loadConfig()
	if err != nil || cfg.Telemetry.Mode != config.TelemetryRelay {
		return
	}
	p := relay.ContentPolicy{Prompts: !cfg.Telemetry.ExcludePrompts, ToolContent: !cfg.Telemetry.ExcludeToolContent}
	if err := relay.SaveContentPolicy(relay.MachineRoute, p); err != nil {
		logf("record the machine content policy: %v", err)
		return
	}
	logf("recorded the machine content policy from setup's choices: prompts %v, tool content %v", p.Prompts, p.ToolContent)
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
	fmt.Fprintf(out, "Backlog:  %d waiting to be delivered to Terma\n", health.Backlog)
	if health.Placing > 0 {
		fmt.Fprintf(out, "Placing:  %d holding records whose session is not placed yet (a trace may wait up to 30 minutes)\n", health.Placing)
	}
	c := health.Counters
	fmt.Fprintf(out, "Since start: %d received, %d routed, %d delivered, %d refused, %d dropped\n",
		c.Received, c.Routed, c.Delivered, c.Dead, c.Dropped)
	if c.Rejected > 0 {
		fmt.Fprintf(out, "Rejected:  %d records the gateway dropped from accepted requests (see the log)\n", c.Rejected)
	}
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

// relayUpdatingEnv tells the `update --refresh` a relay runs after installing a release
// that the relay restarts itself.
const relayUpdatingEnv = "TERMA_RELAY_UPDATING"

// relayUpdater keeps this installation current from the one terma process that is
// always running. Every hour (the first look a minute after start, each look spread by
// up to ten minutes so a fleet does not arrive at once) it reads the signed policy and,
// once a day, the latest release; selfupdate.Background installs one when this
// installation updates itself. Then it refreshes what terma installed, as `terma update`
// does, and stops the relay: launchd or systemd starts it again on the new binary.
func relayUpdater(ctx context.Context, exe string, logf func(string, ...any), restart func()) {
	dir, err := config.Dir()
	if err != nil {
		return
	}
	client := &selfupdate.Client{Version: Version}
	wait := time.Minute
	for {
		t := time.NewTimer(wait + time.Duration(rand.Int64N(int64(10*time.Minute))))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		wait = selfupdate.PolicyInterval
		if os.Getenv("TERMA_NO_UPDATE_CHECK") == "1" {
			continue
		}
		installed := client.Background(ctx, dir, exe, logf)
		if installed == "" {
			continue
		}
		refresh := exec.CommandContext(ctx, exe, "update", "--refresh")
		refresh.Env = append(os.Environ(), relayUpdatingEnv+"=1")
		if out, err := refresh.CombinedOutput(); err != nil {
			logf("refresh after updating to %s: %v: %s", installed, err, strings.TrimSpace(string(out)))
		}
		logf("updated to %s; restarting on it", installed)
		restart()
		return
	}
}
