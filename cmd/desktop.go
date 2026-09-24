package cmd

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/adapter"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/desktoprelay"
	"github.com/miradorlabs/terma-cli/internal/harness"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

const desktopServiceLabel = "ai.terma.codex-relay"

func newDesktopCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "desktop",
		Short:  "Route local Codex desktop logs to each repository's Terma project",
		Hidden: true,
	}
	var manual bool
	connect := &cobra.Command{
		Use:   "connect",
		Short: "Connect Codex desktop to the local per-repository relay",
		Long: `Install a user-level Codex logs exporter pointing at Terma's loopback relay.
Trusted repository SessionStart hooks register conversations with their bound
projects; the relay forwards only records with a matching session and configured
Codex route. Prompt text and tool output are filtered by each repository's route;
sessions without a route stay local. Native aggregate metrics keep their existing
exporter because they have no repository identifier. Restart the desktop app
after connecting so its shared backend loads the new config.`,
		RunE: func(cmd *cobra.Command, _ []string) error { return connectDesktop(cmd, manual) },
	}
	connect.Flags().BoolVar(&manual, "manual", false, "configure Codex but run `terma desktop serve` yourself instead of installing a macOS LaunchAgent")
	cmd.AddCommand(connect)
	cmd.AddCommand(&cobra.Command{
		Use:   "serve",
		Short: "Run the loopback OTLP receiver and per-project delivery queue",
		RunE: func(cmd *cobra.Command, _ []string) error {
			relay, err := desktoprelay.New()
			if err != nil {
				return err
			}
			return relay.Run(cmd.Context())
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show the Codex desktop relay and its pending batches",
		RunE:  statusDesktop,
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "disconnect",
		Short: "Restore Codex's prior exporter and remove the relay service",
		RunE:  disconnectDesktop,
	})
	return cmd
}

func connectDesktop(cmd *cobra.Command, manual bool) error {
	h := harness.Codex{}
	if err := desktopPreflight(); err != nil {
		return err
	}
	started := false
	if !manual {
		if runtime.GOOS != "darwin" {
			return errors.New("automatic desktop service installation is available on macOS; use --manual here")
		}
		if err := installDesktopService(cmd.Context()); err != nil {
			return err
		}
		started = true
		if err := waitDesktopHealth(cmd.Context(), 5*time.Second); err != nil {
			_ = removeDesktopService(cmd.Context())
			return fmt.Errorf("desktop relay did not start: %w", err)
		}
	}
	exporter := harness.Exporter{
		Endpoint: desktoprelay.Endpoint,
		Signals:  []harness.Signal{harness.SignalLogs},
		// Codex applies these switches before the relay sees a record. Leave
		// content available locally so the repository route can enforce its own
		// choices, including --exclude-prompts and --exclude-tool-content.
		IncludePrompts:     true,
		IncludeToolContent: true,
	}
	if err := h.Connect(exporter, false); err != nil {
		if started {
			_ = removeDesktopService(cmd.Context())
		}
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Codex desktop logs now point at Terma's local per-repository relay.")
	if manual {
		fmt.Fprintln(cmd.OutOrStdout(), "Run `terma desktop serve` in a separate terminal.")
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Restart the Codex desktop app to load the exporter, then run `terma desktop status`.")
	return nil
}

func desktopPreflight() error {
	status, err := (harness.Codex{}).Status()
	if err != nil {
		return err
	}
	if status.Connected && status.Endpoint != desktoprelay.Endpoint {
		return fmt.Errorf("codex already exports to %s; disconnect that global export before enabling the desktop relay", status.Endpoint)
	}
	return nil
}

func desktopReceiverRunning() bool {
	response, err := (&http.Client{Timeout: time.Second}).Get(desktoprelay.Endpoint + "/health")
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}

func desktopConfigured() bool {
	status, err := (harness.Codex{}).Status()
	return err == nil && status.Endpoint == desktoprelay.Endpoint &&
		slices.Contains(status.Signals, harness.SignalLogs) && status.IncludePrompts &&
		desktopReceiverRunning()
}

func disconnectDesktop(cmd *cobra.Command, _ []string) error {
	h := harness.Codex{}
	status, err := h.Status()
	if err != nil {
		return err
	}
	if status.Endpoint != "" && status.Endpoint != desktoprelay.Endpoint {
		return fmt.Errorf("codex now exports to %s; refusing to alter that configuration", status.Endpoint)
	}
	if status.Endpoint == desktoprelay.Endpoint {
		if _, err := h.Disconnect(); err != nil {
			return err
		}
	}
	if runtime.GOOS == "darwin" {
		if err := removeDesktopService(cmd.Context()); err != nil {
			return err
		}
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Desktop relay disconnected. Restart the Codex desktop app to stop its previous exporter.")
	return nil
}

func statusDesktop(cmd *cobra.Command, _ []string) error {
	status, err := (harness.Codex{}).Status()
	if err != nil {
		return err
	}
	configured := status.Endpoint == desktoprelay.Endpoint && slices.Contains(status.Signals, harness.SignalLogs)
	response, err := (&http.Client{Timeout: time.Second}).Get(desktoprelay.Endpoint + "/health")
	running := err == nil && response.StatusCode == http.StatusOK
	if response != nil {
		defer response.Body.Close()
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Codex exporter: %s\n", yesNo(configured))
	fmt.Fprintf(cmd.OutOrStdout(), "Prompt text:    %s at exporter\n", onOff(status.IncludePrompts))
	fmt.Fprintf(cmd.OutOrStdout(), "Tool output:    %s at exporter\n", onOff(status.IncludeToolContent))
	fmt.Fprintf(cmd.OutOrStdout(), "Local relay:    %s\n", yesNo(running))
	if running {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		fmt.Fprintf(cmd.OutOrStdout(), "Receiver:       %s\n", strings.TrimSpace(string(body)))
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := termaproject.Find(cwd)
	if errors.Is(err, termaproject.ErrNotFound) {
		fmt.Fprintln(cmd.OutOrStdout(), "Repository:     no Terma project binding in this directory")
		return nil
	}
	if err != nil {
		return err
	}
	binding, err := termaproject.Load(root)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Repository:     %s\n", binding.Project.ID)
	fmt.Fprintf(cmd.OutOrStdout(), "Project route:  %s\n", yesNo(desktoprelay.ReadyForProject(binding.Project.ID)))
	if route, ok, err := shim.LoadRecord(binding.Project.ID); err == nil && ok {
		fmt.Fprintf(cmd.OutOrStdout(), "Repo prompts:   %s\n", onOff(route.IncludePrompts))
		fmt.Fprintf(cmd.OutOrStdout(), "Repo output:    %s\n", onOff(route.IncludeToolContent))
	}
	if codex, ok := adapter.Lookup("codex"); ok {
		if trusting, ok := codex.(adapter.Trusting); ok {
			trust, err := trusting.Trust(root)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Codex hooks:    %s\n", yesNo(trust.Trusted))
			if !trust.Trusted && trust.Fix != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Next:           %s\n", trust.Fix)
			}
		}
	}
	return nil
}

func yesNo(v bool) string {
	if v {
		return "ready"
	}
	return "not ready"
}

func desktopServicePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", desktopServiceLabel+".plist"), nil
}

func plistEscape(s string) string {
	var out bytes.Buffer
	_ = xml.EscapeText(&out, []byte(s))
	return out.String()
}

func installDesktopService(ctx context.Context) error {
	path, err := desktopServicePath()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "desktop-relay"), 0o700); err != nil {
		return err
	}
	// launchd owns startup and restarts a crashed receiver. The queue and hooks use
	// the same config directory even if the installer uses a custom XDG location.
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>desktop</string><string>serve</string></array>
<key>EnvironmentVariables</key><dict><key>TERMA_CONFIG_DIR</key><string>%s</string></dict>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
<key>StandardOutPath</key><string>/dev/null</string>
<key>StandardErrorPath</key><string>/dev/null</string>
</dict></plist>
`, desktopServiceLabel, plistEscape(exe), plistEscape(dir))
	if err := config.WriteFileAtomic(path, []byte(plist), 0o644); err != nil {
		return err
	}
	target := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.CommandContext(ctx, "launchctl", "bootout", target+"/"+desktopServiceLabel).Run()
	if out, err := exec.CommandContext(ctx, "launchctl", "bootstrap", target, path).CombinedOutput(); err != nil {
		return fmt.Errorf("start desktop relay: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func removeDesktopService(ctx context.Context) error {
	path, err := desktopServicePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + desktopServiceLabel
	_ = exec.CommandContext(ctx, "launchctl", "bootout", target).Run()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func waitDesktopHealth(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		response, err := client.Get(desktoprelay.Endpoint + "/health")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("no health response from the loopback listener")
}
