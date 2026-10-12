package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay/service"
)

// The per-user service catches what an agent exports before its first hook, which a
// hook-started relay cannot. It restarts on a nonzero exit (a crash, or ExitRestart after
// a replaced binary); a relay that exits 0 found its setup gone and stays stopped.

func serviceName(stateDir string) string {
	def, _ := config.DefaultStateDir()
	return service.Label(stateDir, def)
}

// relayEnvKeys place the config and state directories and pick the backend: all a relay
// takes of whoever starts it.
var relayEnvKeys = []string{"TERMA_CONFIG_DIR", "TERMA_STATE_DIR", "TERMA_ENV", "XDG_CONFIG_HOME", "XDG_STATE_HOME",
	"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "TERMA_RELAY_HOLD"}

// callerEnv is this process's relayEnvKeys, nothing else of the caller's.
func callerEnv() map[string]string {
	env := map[string]string{}
	for _, k := range relayEnvKeys {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return env
}

// RecordEnv records this process's environment as the one every relay of this config
// directory runs in. Only a developer's own commands record it (install, setup, relay
// daemon install), since a hook or a stray shell may lack TERMA_ENV: a relay that inherits
// such an environment talks to another backend and forwards nothing.
func RecordEnv(dir string) error {
	data, err := json.MarshalIndent(callerEnv(), "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(dir, EnvFile), append(data, '\n'), 0o600)
}

// RecordedEnv is the environment RecordEnv last recorded, if any.
func RecordedEnv(dir string) (map[string]string, bool) {
	data, err := os.ReadFile(filepath.Join(dir, EnvFile))
	if err != nil {
		return nil, false
	}
	env := map[string]string{}
	if json.Unmarshal(data, &env) != nil {
		return nil, false
	}
	return env, true
}

// relayEnv is the recorded environment, else this process's.
func relayEnv(dir string) map[string]string {
	if env, ok := RecordedEnv(dir); ok {
		return env
	}
	return callerEnv()
}

// withRelayEnv is base with each relayEnvKeys set exactly as env has it, and unset where env
// lacks it: a missing TERMA_ENV means production, not the caller's choice.
func withRelayEnv(base []string, env map[string]string) []string {
	out := make([]string, 0, len(base)+len(env))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(relayEnvKeys, k) {
			out = append(out, kv)
		}
	}
	for _, k := range relayEnvKeys {
		if v, ok := env[k]; ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// Service is the relay service of the state directory stateDir, with this process's
// environment; Exe is left to install.
func Service(stateDir string) (service.Manager, error) {
	return serviceWith(stateDir, callerEnv())
}

func serviceWith(stateDir string, env map[string]string) (service.Manager, error) {
	dir, err := Dir(stateDir)
	if err != nil {
		return service.Manager{}, err
	}
	return service.Manager{Name: serviceName(stateDir), RelayDir: dir, Env: env, StopRelay: func() { Stop(dir) }}, nil
}

// InstallService writes and starts the relay service for this state directory, in this
// process's environment, which it records for every relay after it.
func InstallService(ctx context.Context, stateDir string) (string, error) {
	m, err := Service(stateDir)
	if err != nil {
		return "", err
	}
	path, err := install(ctx, stateDir, m)
	if err != nil {
		return "", err
	}
	return path, RecordEnv(m.RelayDir)
}

// install installs m as this terma, for the state directory stateDir.
func install(ctx context.Context, stateDir string, m service.Manager) (string, error) {
	if !service.Supported() {
		return "", fmt.Errorf("a relay service is not supported on %s; hooks start the relay on demand", runtime.GOOS)
	}
	m, err := asThisTerma(m)
	if err != nil {
		return "", err
	}
	if _, err := Token(stateDir); err != nil {
		return "", err
	}
	return m.Install(ctx)
}

// asThisTerma is m running this terma, with its Windows supervisor.
func asThisTerma(m service.Manager) (service.Manager, error) {
	var err error
	if m.Exe, err = procinfo.AbsExecutable(); err != nil {
		return m, err
	}
	m.StartSupervisor = func() error {
		sup := exec.Command(m.Exe, "relay", "supervise")
		procinfo.Detach(sup)
		if err := sup.Start(); err != nil {
			return err
		}
		return sup.Process.Release()
	}
	return m, nil
}

// StartService starts the installed service's relay as it is defined, without rewriting the
// definition: a definition that is current needs its relay running, not a restart.
func StartService(ctx context.Context, stateDir string) error {
	m, err := thisService(stateDir)
	if err != nil {
		return err
	}
	return m.Start(ctx)
}

// RestartService starts the service's relay again at once after Stop.
func RestartService(ctx context.Context, stateDir string) error {
	m, err := thisService(stateDir)
	if err != nil {
		return err
	}
	return m.Restart(ctx)
}

// thisService is the service of the state directory stateDir, running this terma.
func thisService(stateDir string) (service.Manager, error) {
	m, err := Service(stateDir)
	if err != nil {
		return m, err
	}
	return asThisTerma(m)
}

// ServiceState is the relay service as installed: where its definition is, and whether it
// is the one this terma would install now.
type ServiceState struct {
	Path               string
	Installed, Current bool
}

// CheckService compares the installed service with what this terma would install in the
// recorded environment: what `terma update` refreshes and doctor reports.
func CheckService(stateDir string) ServiceState {
	dir, err := Dir(stateDir)
	if err != nil {
		return ServiceState{}
	}
	return checkService(stateDir, relayEnv(dir))
}

// CheckServiceHere compares it with what this process would install, in its own
// environment: what `terma setup` repairs.
func CheckServiceHere(stateDir string) ServiceState {
	return checkService(stateDir, callerEnv())
}

func checkService(stateDir string, env map[string]string) ServiceState {
	m, err := serviceWith(stateDir, env)
	if err != nil {
		return ServiceState{}
	}
	if m.Exe, err = procinfo.AbsExecutable(); err != nil {
		return ServiceState{}
	}
	path, installed := m.Installed()
	return ServiceState{Path: path, Installed: installed, Current: installed && m.Current()}
}

// RefreshService rewrites and restarts an installed service that is not what this terma
// would install in the recorded environment, reporting whether it did. A service that is
// not installed stays so: removing it was the developer's choice.
func RefreshService(ctx context.Context, stateDir string) (string, bool, error) {
	dir, err := Dir(stateDir)
	if err != nil {
		return "", false, err
	}
	env := relayEnv(dir)
	state := checkService(stateDir, env)
	if !state.Installed || state.Current {
		return state.Path, false, nil
	}
	m, err := serviceWith(stateDir, env)
	if err != nil {
		return "", false, err
	}
	path, err := install(ctx, stateDir, m)
	if err != nil {
		return state.Path, false, err
	}
	return path, true, nil
}

// RemoveService stops and removes the relay service. A service relay still running after
// (the service manager's stop failed) is stopped here: it never idles, so it would keep the
// port with no service left to remove.
func RemoveService(ctx context.Context, stateDir string) (bool, error) {
	m, err := Service(stateDir)
	if err != nil {
		return false, err
	}
	removed, err := m.Remove(ctx)
	if running, ok := RunningRelay(m.RelayDir); ok && running.Launch == LaunchService {
		Stop(m.RelayDir)
	}
	return removed, err
}

// ServiceInstalled reports whether this state directory has a relay service definition, and where.
func ServiceInstalled(stateDir string) (string, bool) {
	m, err := Service(stateDir)
	if err != nil {
		return "", false
	}
	return m.Installed()
}
