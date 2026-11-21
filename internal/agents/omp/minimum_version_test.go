package omp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ompOnPath puts an omp whose --version prints version first on PATH.
func ompOnPath(t *testing.T, version string) {
	t.Helper()
	bin := t.TempDir()
	write(t, filepath.Join(bin, "omp"), "#!/bin/sh\necho omp/"+version+"\n", 0o700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// OMP before 19 loads its agent .env into its own environment, so every tool it runs would
// inherit the relay's endpoints and their token: setup leaves such an OMP alone, and doctor
// fails it, and setup does not pick it as installed. An OMP 19 or later, or one setup cannot
// run, is set up as before.
func TestOMPBefore19IsRefused(t *testing.T) {
	for _, c := range []struct {
		version string
		refused bool
	}{{"18.0.0", true}, {"18.9.4-beta.1", true}, {"19.0.0", false}, {"19.1.0", false}, {"20.0.1", false}} {
		t.Run(c.version, func(t *testing.T) {
			a, env := ompIn(t)
			ompOnPath(t, c.version)
			// Setup selects what is installed for --yes: an OMP it would refuse is not, so it
			// cannot stop the other agents' setup; asked for by name, it is refused.
			if installed := a.Installed(context.Background()); installed == c.refused {
				t.Errorf("installed %v for OMP %s", installed, c.version)
			}
			_, err := a.ConfigureRelay(context.Background(), relayConfig())
			if refused := err != nil; refused != c.refused {
				t.Fatalf("setup error %v, want refused %v", err, c.refused)
			}
			if !c.refused {
				detail, _, problem := a.RelayProblem("127.0.0.1:43180")
				if problem {
					t.Errorf("doctor reports a problem: %s", detail)
				}
				return
			}
			if !strings.Contains(err.Error(), "OMP 19 or later is required") || !strings.Contains(err.Error(), c.version) {
				t.Errorf("setup said %q", err)
			}
			if _, statErr := os.Stat(env); !os.IsNotExist(statErr) {
				t.Errorf("setup wrote the .env of an OMP it refused")
			}
			if dir, _ := ompAgentDir(); dir != "" {
				if _, statErr := os.Stat(filepath.Join(dir, extensionPath)); !os.IsNotExist(statErr) {
					t.Errorf("setup wrote the extension of an OMP it refused")
				}
			}
			detail, fix, problem := a.RelayProblem("127.0.0.1:43180")
			if !problem || !strings.Contains(detail, "OMP 19 or later is required") || fix == "" {
				t.Errorf("doctor: problem %v %q %q", problem, detail, fix)
			}
		})
	}
}
