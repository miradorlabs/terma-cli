package pi

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// piOnPath makes the PATH a folder holding only a pi whose --version prints version, or
// nothing at all for "": no test runs the machine's own Pi.
func piOnPath(t *testing.T, version string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs an executable shim on PATH")
	}
	bin := t.TempDir()
	if version != "" {
		if err := os.WriteFile(filepath.Join(bin, "pi"), []byte("#!/bin/sh\necho "+version+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
}

// The extension's own check and setup's refuse the same Pi.
func TestMinimumVersionMatchesTheExtension(t *testing.T) {
	m := regexp.MustCompile(`(?m)^const MIN_PI = "([^"]+)"$`).FindStringSubmatch(extensionTemplate)
	if m == nil || m[1] != minimumVersion {
		t.Fatalf("terma.ts MIN_PI = %v, minimumVersion = %s", m, minimumVersion)
	}
}

// A Pi before 1.5.0 (a pre-release suffix ignored, as the extension ignores it), the retired
// 0.x package included, is not installed for setup --yes, is refused by name before anything
// is written, and is doctor's first problem; a Pi 1.5.0 or later, or one whose version cannot
// be read, is set up.
func TestPiBefore1_5_0IsRefused(t *testing.T) {
	for _, c := range []struct {
		version string
		refused bool
	}{
		{"0.73.1", true}, {"1.4.9", true}, {"1.4.99-rc.1", true},
		{"1.5.0", false}, {"1.5.0-beta.1", false}, {"1.6.1", false}, {"2.0.0", false}, {"nightly", false},
	} {
		t.Run(c.version, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PI_CODING_AGENT_DIR", dir)
			piOnPath(t, c.version)
			a := Agent{}
			if installed := a.Installed(context.Background()); installed == c.refused {
				t.Errorf("installed %v", installed)
			}
			_, err := a.ConfigureRelay(context.Background(), agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "tok", HookCommand: []string{"/bin/sh", "hook"}})
			if refused := err != nil; refused != c.refused {
				t.Fatalf("setup error %v, want refused %v", err, c.refused)
			}
			if !c.refused {
				return
			}
			want := "Pi 1.5.0 or later is required (found " + c.version + "): terma's extension records nothing on an earlier Pi. Upgrade Pi (pi update, or npm install -g @earendil-works/pi-coding-agent)"
			if err.Error() != want {
				t.Errorf("setup said %q", err)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "extensions", "terma.ts")); !os.IsNotExist(statErr) {
				t.Errorf("setup wrote the extension of a Pi it refused")
			}
			detail, fix, problem := a.RelayProblem("127.0.0.1:43180")
			if !problem || !strings.HasPrefix(detail, "Pi 1.5.0 or later is required") || fix != "upgrade Pi, then run terma setup" {
				t.Errorf("doctor: problem %v %q %q", problem, detail, fix)
			}
		})
	}
}
