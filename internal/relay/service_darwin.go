package relay

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/miradorlabs/terma-cli/internal/config"
)

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

func launchdDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func launchdTarget() string { return launchdDomain() + "/" + launchdLabel }

func installService(ctx context.Context, binary string) error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	want := launchdPlist(binary, serviceEnv())
	have, _ := os.ReadFile(path)
	state, _ := serviceState(ctx)
	if bytes.Equal(have, want) && state.Running {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := config.WriteFileAtomic(path, want, 0o644); err != nil {
		return err
	}
	// bootout fails when the agent is not loaded, which is the first install.
	_, _ = runCommand(ctx, "launchctl", "bootout", launchdTarget())
	_, err = runCommand(ctx, "launchctl", "bootstrap", launchdDomain(), path)
	return err
}

func uninstallService(ctx context.Context) error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	_, _ = runCommand(ctx, "launchctl", "bootout", launchdTarget())
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func restartService(ctx context.Context) error {
	state, err := serviceState(ctx)
	if err != nil || !state.Installed {
		return err
	}
	if !state.Loaded {
		path, err := plistPath()
		if err != nil {
			return err
		}
		_, err = runCommand(ctx, "launchctl", "bootstrap", launchdDomain(), path)
		return err
	}
	_, err = runCommand(ctx, "launchctl", "kickstart", "-k", launchdTarget())
	return err
}

var (
	launchdPID   = regexp.MustCompile(`(?m)^\s*pid = (\d+)`)
	launchdState = regexp.MustCompile(`(?m)^\s*state = running`)
)

func serviceState(ctx context.Context) (ServiceState, error) {
	path, err := plistPath()
	if err != nil {
		return ServiceState{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ServiceState{}, nil
	}
	if err != nil {
		return ServiceState{}, err
	}
	s := ServiceState{Installed: true}
	if m := plistBinaryRe.FindSubmatch(data); m != nil {
		s.Binary = xmlUnescape(string(m[1]))
	}
	out, err := runCommand(ctx, "launchctl", "print", launchdTarget())
	if err != nil {
		return s, nil
	}
	s.Loaded = true
	s.Running = launchdState.Match(out) && launchdPID.Match(out)
	return s, nil
}
