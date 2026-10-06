package config

import (
	"cmp"
	"path/filepath"
	"runtime"
	"testing"
)

// TestDirsResolveInOrder runs on every CI platform, the Windows job included: the
// %APPDATA% and %LOCALAPPDATA% cases expect them only there.
func TestDirsResolveInOrder(t *testing.T) {
	home := t.TempDir()
	onWindows := func(windows, other string) string {
		if runtime.GOOS == "windows" {
			return windows
		}
		return other
	}
	for _, tc := range []struct {
		name                  string
		env                   map[string]string
		wantConfig, wantState string
		// wantDefault is DefaultStateDir, which ignores the TERMA_ variables; empty means wantState.
		wantDefault string
	}{{
		name:       "home defaults",
		wantConfig: filepath.Join(home, ".config", "terma"),
		wantState:  filepath.Join(home, ".local", "state", "terma"),
	}, {
		name:       "Windows folders",
		env:        map[string]string{"APPDATA": filepath.Join(home, "Roaming"), "LOCALAPPDATA": filepath.Join(home, "Local")},
		wantConfig: onWindows(filepath.Join(home, "Roaming", "terma"), filepath.Join(home, ".config", "terma")),
		wantState:  onWindows(filepath.Join(home, "Local", "terma"), filepath.Join(home, ".local", "state", "terma")),
	}, {
		name: "XDG over the Windows folders",
		env: map[string]string{"XDG_CONFIG_HOME": filepath.Join(home, "xdg-config"), "XDG_STATE_HOME": filepath.Join(home, "xdg-state"),
			"APPDATA": filepath.Join(home, "Roaming"), "LOCALAPPDATA": filepath.Join(home, "Local")},
		wantConfig: filepath.Join(home, "xdg-config", "terma"),
		wantState:  filepath.Join(home, "xdg-state", "terma"),
	}, {
		name:        "a sandboxed config directory holds the state too",
		env:         map[string]string{"TERMA_CONFIG_DIR": filepath.Join(home, "sandbox"), "XDG_STATE_HOME": filepath.Join(home, "xdg-state")},
		wantConfig:  filepath.Join(home, "sandbox"),
		wantState:   filepath.Join(home, "sandbox"),
		wantDefault: filepath.Join(home, "xdg-state", "terma"),
	}, {
		name:        "TERMA_STATE_DIR alone",
		env:         map[string]string{"TERMA_STATE_DIR": filepath.Join(home, "state")},
		wantConfig:  filepath.Join(home, ".config", "terma"),
		wantState:   filepath.Join(home, "state"),
		wantDefault: filepath.Join(home, ".local", "state", "terma"),
	}, {
		name:        "TERMA_STATE_DIR over everything",
		env:         map[string]string{"TERMA_CONFIG_DIR": filepath.Join(home, "sandbox"), "TERMA_STATE_DIR": filepath.Join(home, "state")},
		wantConfig:  filepath.Join(home, "sandbox"),
		wantState:   filepath.Join(home, "state"),
		wantDefault: filepath.Join(home, ".local", "state", "terma"),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"TERMA_CONFIG_DIR", "TERMA_STATE_DIR", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "APPDATA", "LOCALAPPDATA"} {
				t.Setenv(k, tc.env[k])
			}
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			if got, err := Dir(); err != nil || got != tc.wantConfig {
				t.Errorf("Dir() = %q, %v; want %q", got, err, tc.wantConfig)
			}
			if got, err := StateDir(); err != nil || got != tc.wantState {
				t.Errorf("StateDir() = %q, %v; want %q", got, err, tc.wantState)
			}
			// The relay service's label is its state directory's hash unless it is this one.
			want := cmp.Or(tc.wantDefault, tc.wantState)
			if got, err := DefaultStateDir(); err != nil || got != want {
				t.Errorf("DefaultStateDir() = %q, %v; want %q", got, err, want)
			}
		})
	}
}
