package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// The relay as a per-user service: launchd on macOS, systemd --user on Linux. `terma
// install` sets it up by default (relayServiceWanted). Without it hooks start a relay on
// demand, which cannot close two gaps: what an agent exports before its first hook
// (Codex's conversation_starts) when no relay runs, and anything exported while none
// does. The service runs `terma relay run --idle 0` and is restarted by the service
// manager when it exits nonzero — a crash, or ExitRestart when the relay steps aside for
// a replaced binary, so an update takes effect without anyone restarting it. A relay
// that exits 0 found its setup gone (terma uninstalled) and stays stopped.

// relayServiceName is the service's label: one per config directory, so a relay for a
// sandboxed config (tests, a second profile directory) never collides with the real one.
func relayServiceName() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	name := "ai.terma.relay"
	if def, err := defaultConfigDir(); err != nil || filepath.Clean(dir) != filepath.Clean(def) {
		sum := sha256.Sum256([]byte(filepath.Clean(dir)))
		name += "." + hex.EncodeToString(sum[:])[:10]
	}
	return name, nil
}

func defaultConfigDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "terma"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "terma"), nil
}

// serviceEnv is the environment the service runs with: what places its config and its
// environment, nothing else of the caller's.
func serviceEnv() map[string]string {
	env := map[string]string{}
	for _, k := range []string{"TERMA_CONFIG_DIR", "TERMA_ENV", "XDG_CONFIG_HOME", "HOME", "TERMA_RELAY_HOLD"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return env
}

// relayServiceExecutable is the terma the service runs: the path this one was started
// as, not its resolved target — a package manager's upgrade removes the old target, and
// the service must start whatever the stable path points at next.
func relayServiceExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(exe)
}

func launchdPlist(label, exe, logPath string, env map[string]string) string {
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + esc(label) + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + esc(exe) + `</string><string>relay</string><string>run</string><string>--idle</string><string>0</string><string>--quiet</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
`)
	for _, k := range sortedKeys(env) {
		b.WriteString("    <key>" + esc(k) + "</key><string>" + esc(env[k]) + "</string>\n")
	}
	b.WriteString(`  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>` + esc(logPath) + `</string>
  <key>StandardErrorPath</key><string>` + esc(logPath) + `</string>
</dict>
</plist>
`)
	return b.String()
}

func systemdUnit(exe string, env map[string]string) string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=terma local OTLP relay\n\n[Service]\n")
	b.WriteString("ExecStart=" + strconv.Quote(exe) + " relay run --idle 0 --quiet\n")
	for _, k := range sortedKeys(env) {
		b.WriteString("Environment=" + strconv.Quote(k+"="+env[k]) + "\n")
	}
	b.WriteString("Restart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

// relayServicePath is where the service definition lives.
func relayServicePath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", name+".plist"), nil
	case "linux":
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			base = filepath.Join(home, ".config")
		}
		return filepath.Join(base, "systemd", "user", name+".service"), nil
	}
	return "", fmt.Errorf("a relay service is not supported on %s; hooks start the relay on demand", runtime.GOOS)
}

func runService(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// launchdDomains are the launchd domains to try: a login session's, then the user's
// background one (a machine with nobody logged in at the console, such as CI).
func launchdDomains() []string {
	uid := strconv.Itoa(os.Getuid())
	return []string{"gui/" + uid, "user/" + uid}
}

func installRelayService(ctx context.Context) (string, error) {
	name, err := relayServiceName()
	if err != nil {
		return "", err
	}
	path, err := relayServicePath(name)
	if err != nil {
		return "", err
	}
	exe, err := relayServiceExecutable()
	if err != nil {
		return "", err
	}
	dir, err := relayDir()
	if err != nil {
		return "", err
	}
	if _, err := relayToken(); err != nil {
		return "", err
	}
	// A relay a hook started holds the port and the lock: the service takes over.
	stopRelay(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		plist := launchdPlist(name, exe, filepath.Join(dir, "daemon.log"), serviceEnv())
		if err := config.WriteFileAtomic(path, []byte(plist), 0o644); err != nil {
			return "", err
		}
		var lastErr error
		for _, domain := range launchdDomains() {
			_, _ = runService(ctx, "launchctl", "bootout", domain+"/"+name)
			out, err := runService(ctx, "launchctl", "bootstrap", domain, path)
			if err == nil {
				return path, nil
			}
			lastErr = fmt.Errorf("launchctl bootstrap %s: %w: %s", domain, err, out)
		}
		return "", lastErr
	case "linux":
		if err := config.WriteFileAtomic(path, []byte(systemdUnit(exe, serviceEnv())), 0o644); err != nil {
			return "", err
		}
		if out, err := runService(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return "", fmt.Errorf("systemctl --user daemon-reload: %w: %s", err, out)
		}
		if out, err := runService(ctx, "systemctl", "--user", "enable", "--now", name+".service"); err != nil {
			return "", fmt.Errorf("systemctl --user enable: %w: %s", err, out)
		}
		return path, nil
	}
	return "", errors.New("unsupported")
}

func removeRelayService(ctx context.Context) (bool, error) {
	name, err := relayServiceName()
	if err != nil {
		return false, err
	}
	path, err := relayServicePath(name)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	}
	switch runtime.GOOS {
	case "darwin":
		for _, domain := range launchdDomains() {
			_, _ = runService(ctx, "launchctl", "bootout", domain+"/"+name)
		}
	case "linux":
		_, _ = runService(ctx, "systemctl", "--user", "disable", "--now", name+".service")
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	if runtime.GOOS == "linux" {
		_, _ = runService(ctx, "systemctl", "--user", "daemon-reload")
	}
	return true, nil
}

// relayServiceInstalled reports whether this config directory's relay runs as a
// service, and where its definition is.
func relayServiceInstalled() (string, bool) {
	name, err := relayServiceName()
	if err != nil {
		return "", false
	}
	path, err := relayServicePath(name)
	if err != nil {
		return "", false
	}
	_, err = os.Stat(path)
	return path, err == nil
}

func newRelayDaemonCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the relay as a per-user service (launchd, systemd --user)",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "install",
		Short: "Install and start the relay service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			relayServiceWanted("on") // an explicit install clears an opt-out
			path, err := installRelayService(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "The relay runs as a service (%s): always on, restarted by the system.\n", path)
			return nil
		},
	}, &cobra.Command{
		Use:   "remove",
		Short: "Stop and remove the relay service (hooks start the relay on demand again)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			removed, err := removeRelayService(cmd.Context())
			if err != nil {
				return err
			}
			relayServiceWanted("off") // and install does not put it back
			if removed {
				fmt.Fprintln(cmd.OutOrStdout(), "The relay service is removed; hooks start the relay on demand.")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "No relay service was installed.")
			}
			return nil
		},
	})
	return cmd
}
