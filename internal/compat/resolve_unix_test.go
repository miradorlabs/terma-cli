//go:build unix

package compat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Cache and version behavior is independent of how heavily the host is loaded.
// Production deadline behavior is covered through the actual launcher tests.
func resolveForTest(installation Installation) Profile {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return resolve(ctx, installation)
}

func codexSupportsNoDaemon() bool {
	p := resolveForTest(Installation{Harness: "codex", Surface: CLI, Path: filepath.Join(os.Getenv("PATH"), "codex")})
	return p.Capability(CodexNoDaemon).Support == Supported
}

func capabilityFixture(t *testing.T, help string) (binary, log string) {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	bin := t.TempDir()
	binary, log = filepath.Join(bin, "codex"), filepath.Join(bin, "probes")
	t.Setenv("PATH", bin)
	t.Setenv("TERMA_TEST_PROBE_LOG", log)
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then printf 'development\\n'; exit 0; fi\nprintf x >> \"$TERMA_TEST_PROBE_LOG\"\n"+help+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, log
}

func assertProbeCount(t *testing.T, log string, want int) {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil || len(data) != want {
		t.Fatalf("probe count = %d, %v; want %d", len(data), err, want)
	}
}

func TestCodexCapabilityCachesBothAnswers(t *testing.T) {
	for _, supported := range []bool{true, false} {
		t.Run(fmt.Sprint(supported), func(t *testing.T) {
			help := "printf 'Options:\\n  --no-alt-screen\\n'"
			if supported {
				help = "printf 'Options:\\n  --no-daemon\\n'"
			}
			_, log := capabilityFixture(t, help)
			for range 2 {
				if got := codexSupportsNoDaemon(); got != supported {
					t.Fatalf("support = %v, want %v", got, supported)
				}
			}
			assertProbeCount(t, log, 1)
		})
	}
}

func TestCodexCapabilityRetriesFailures(t *testing.T) {
	for _, failure := range []string{"exit 1", "exec /bin/sleep 10"} {
		t.Run(failure, func(t *testing.T) {
			_, log := capabilityFixture(t, "if [ ! -f \"$TERMA_TEST_READY\" ]; then "+failure+"; fi\nprintf '  --no-daemon\\n'")
			ready := filepath.Join(t.TempDir(), "ready")
			t.Setenv("TERMA_TEST_READY", ready)
			if codexSupportsNoDaemon() {
				t.Fatal("failed probe reported support")
			}
			if err := os.WriteFile(ready, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			// The executable is unchanged: only an uncached failure permits retry.
			for range 2 {
				if !codexSupportsNoDaemon() {
					t.Fatal("successful retry was not cached")
				}
			}
			assertProbeCount(t, log, 2)
		})
	}
}

func TestCodexCapabilityInvalidatesExecutableChanges(t *testing.T) {
	for _, change := range []string{"mtime", "size", "path", "symlink"} {
		t.Run(change, func(t *testing.T) {
			binary, log := capabilityFixture(t, "printf '  --no-daemon\\n'")
			link := filepath.Join(t.TempDir(), "codex")
			if change == "symlink" {
				if err := os.Symlink(binary, link); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", filepath.Dir(link))
			}
			if !codexSupportsNoDaemon() {
				t.Fatal("initial probe failed")
			}
			info, err := os.Stat(binary)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(binary)
			if err != nil {
				t.Fatal(err)
			}
			// Equal-length replacement isolates mtime invalidation from size.
			data = []byte(strings.ReplaceAll(string(data), "--no-daemon", "--no-xxxxxx"))
			if change == "size" {
				data = append(data, []byte("# newer binary\n")...)
			}
			target := binary
			if change == "path" || change == "symlink" {
				target = filepath.Join(t.TempDir(), "codex")
			}
			if err := os.WriteFile(target, data, 0o755); err != nil {
				t.Fatal(err)
			}
			modified := info.ModTime()
			if change == "mtime" {
				modified = modified.Add(time.Second)
			}
			if err := os.Chtimes(target, modified, modified); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "path":
				t.Setenv("PATH", filepath.Dir(target))
			case "symlink":
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
			}
			if codexSupportsNoDaemon() {
				t.Fatal("stale capability survived an executable change")
			}
			assertProbeCount(t, log, 2)
		})
	}
}

func TestCodexCapabilityCacheFailuresAreBestEffort(t *testing.T) {
	for _, broken := range []string{"corrupt", "unwritable"} {
		t.Run(broken, func(t *testing.T) {
			binary, log := capabilityFixture(t, "printf '  --no-daemon\\n'")
			identity, err := binaryIdentity(binary)
			if err != nil {
				t.Fatal(err)
			}
			path := capabilityCachePath(identity.Path)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if broken == "corrupt" {
				if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				// A directory cannot be replaced by the atomic cache-file rename.
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if !codexSupportsNoDaemon() {
					t.Fatal("cache failure prevented capability detection")
				}
			}
			want := 1
			if broken == "unwritable" {
				want = 2
			}
			assertProbeCount(t, log, want)
		})
	}
}

func TestResolveUsesVersionRulesWithoutHelp(t *testing.T) {
	for _, version := range []string{"0.155.1", "0.156.0", "0.157.1"} {
		t.Run(version, func(t *testing.T) {
			binary, log := capabilityFixture(t, "exit 9")
			body := "#!/bin/sh\nprintf x >> \"$TERMA_TEST_PROBE_LOG\"\n[ \"$1\" = --version ] || exit 9\nprintf 'codex-cli " + version + "\\n'\n"
			if err := os.WriteFile(binary, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				p := resolveForTest(Installation{Harness: "codex", Surface: CLI, Path: binary})
				want := ForVersion(Installation{Harness: "codex", Surface: CLI, Version: version}).Capability(CodexNoDaemon)
				if p.Installation.Version != version || p.Capability(CodexNoDaemon) != want {
					t.Fatalf("profile = %+v", p)
				}
			}
			assertProbeCount(t, log, 1)
		})
	}
}

func TestResolveUnknownVersionsUseHelp(t *testing.T) {
	for _, version := range []string{"0.156.0-alpha.1", "0.999.0", "0.156.0+backport"} {
		t.Run(version, func(t *testing.T) {
			binary, _ := capabilityFixture(t, "printf '  --no-daemon\\n'")
			data, err := os.ReadFile(binary)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.ReplaceAll(string(data), "development", "codex-cli "+version))
			if err := os.WriteFile(binary, data, 0o755); err != nil {
				t.Fatal(err)
			}
			p := resolveForTest(Installation{Harness: "codex", Surface: CLI, Path: binary})
			if p.Installation.Version != version || p.Capability(CodexNoDaemon).Support != Supported || p.Capability(CodexNoDaemon).Source != "probe" {
				t.Fatalf("profile = %+v", p)
			}
		})
	}
}

func TestResolveFailedProbeRemainsUnknown(t *testing.T) {
	binary, _ := capabilityFixture(t, "exit 1")
	p := resolveForTest(Installation{Harness: "codex", Surface: CLI, Path: binary, Version: "0.999.0"})
	if c := p.Capability(CodexNoDaemon); c.Support != Unknown || c.Reason == "" {
		t.Fatalf("failure became %+v", c)
	}
}

func TestResolveDesktopNeverRunsCLIProbe(t *testing.T) {
	binary, log := capabilityFixture(t, "printf '  --no-daemon\\n'")
	p := resolveForTest(Installation{Harness: "codex", Surface: Desktop, Path: binary, Version: "0.156.0"})
	if p.Capability(CodexNoDaemon).Support != Unknown {
		t.Fatal("desktop inherited CLI capabilities")
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("desktop ran a CLI probe")
	}
}

func TestResolveReevaluatesCachedVersion(t *testing.T) {
	binary, log := capabilityFixture(t, "exit 9")
	identity, err := binaryIdentity(binary)
	if err != nil {
		t.Fatal(err)
	}
	// The cache stores detected facts, not the conclusion from an older ruleset.
	saveCache(capabilityCachePath(identity.Path), capabilityCache{Schema: 1, Binary: identity, Version: "0.156.0"})
	p := resolveForTest(Installation{Harness: "codex", Surface: CLI, Path: binary})
	if p.Capability(CodexNoDaemon).Support != Supported || p.Capability(CodexNoDaemon).Source != "version rule" {
		t.Fatalf("profile = %+v", p)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("cached version needed a subprocess")
	}
}

func TestResolveHonorsCancellation(t *testing.T) {
	binary, _ := capabilityFixture(t, "printf '  --no-daemon\\n'")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := Resolve(ctx, Installation{Harness: "codex", Surface: CLI, Path: binary})
	if p.Capability(CodexNoDaemon).Support != Unknown {
		t.Fatal("cancelled detection must remain unknown")
	}
}
