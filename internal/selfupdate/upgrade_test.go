package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// termaScript is a terma that only reports version, as `terma version` does.
func termaScript(t *testing.T, path, version string) {
	t.Helper()
	script(t, path, "echo 'terma "+version+"'")
}

// script writes an executable shell script at path running body.
func script(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// brewed is a Homebrew prefix with terma at 1.0.0 installed as a cask, bin/terma linked to it.
type brewed struct{ prefix, exe, link string }

func newBrewed(t *testing.T) brewed {
	t.Helper()
	unixLayouts(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := brewed{prefix: filepath.Join(root, "brew")}
	b.exe, b.link = filepath.Join(b.prefix, "Caskroom", "terma", "1.0.0", "terma"), filepath.Join(b.prefix, "bin", "terma")
	termaScript(t, b.exe, "1.0.0")
	if err := os.MkdirAll(filepath.Dir(b.link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(b.exe, b.link); err != nil {
		t.Fatal(err)
	}
	return b
}

// brew is the prefix's brew, which runs then once it has recorded how it was run, as
// <prefix>/ran.*.
func (b brewed) brew(t *testing.T, then string) {
	t.Helper()
	ran := filepath.Join(b.prefix, "ran")
	script(t, filepath.Join(b.prefix, "bin", "brew"), fmt.Sprintf(`env | grep '^HOMEBREW_' | sort > %[1]s.env
echo "$@" > %[1]s.args
if [ -t 0 ]; then echo terminal > %[1]s.stdin; else cat > %[1]s.stdin; fi
%[2]s`, ran, then))
}

// upgradeTo is a brew's upgrade of the cask to version, moving bin/terma to it.
func (b brewed) upgradeTo(version string) string {
	next := filepath.Join(b.prefix, "Caskroom", "terma", version, "terma")
	return fmt.Sprintf("mkdir -p %s && printf '#!/bin/sh\\necho \"terma %s\"\\n' > %s && chmod +x %s && rm -rf %s && ln -sf %s %s",
		filepath.Dir(next), version, next, next, filepath.Dir(b.exe), next, b.link)
}

func (b brewed) ran(t *testing.T, what string) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(b.prefix, "ran."+what))
	return string(data)
}

func (b brewed) manager(t *testing.T) Manager {
	t.Helper()
	m, ok := ManagedBy(b.exe)
	if !ok || len(m.Argv) == 0 || m.Terma != b.link {
		t.Fatalf("ManagedBy(%s) = %+v, %v", b.exe, m, ok)
	}
	return m
}

// Homebrew upgrades terma in the background with no terminal and no prompt, its taps updated
// first, the developer's other packages left alone; success is the version the link it
// moved now runs, never its exit status.
func TestHomebrewUpgradesTermaInTheBackground(t *testing.T) {
	for _, tc := range []struct {
		name    string
		then    func(b brewed) string
		want    string
		offered bool
	}{
		{"upgraded", func(b brewed) string { return b.upgradeTo("1.0.1") }, "1.0.1", true},
		{"upgraded, then failed", func(b brewed) string { return b.upgradeTo("1.0.1") + "; echo 'Error: completions' >&2; exit 1" }, "1.0.1", true},
		// Its tap was not updated: it says terma is up to date.
		{"tap without it yet", func(brewed) string {
			return "echo 'Warning: Not upgrading terma, the latest version is already installed'"
		}, "", false},
		{"untrusted tap", func(brewed) string { return "echo 'Error: Refusing to load untrusted tap miradorlabs/tap' >&2; exit 1" }, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBrewed(t)
			b.brew(t, tc.then(b))
			var out bytes.Buffer
			got, err := b.manager(t).Upgrade(t.Context(), "1.0.1", &out)
			if got != tc.want || (err == nil) != (tc.want != "") || errors.Is(err, ErrNotOffered) == tc.offered {
				t.Fatalf("Upgrade = %q, %v; want %q, offered %v\n%s", got, err, tc.want, tc.offered, &out)
			}
			if args := strings.TrimSpace(b.ran(t, "args")); args != "upgrade --cask terma" {
				t.Errorf("brew ran with %q", args)
			}
			if stdin := b.ran(t, "stdin"); stdin != "" {
				t.Errorf("brew's input was %q, want empty", stdin)
			}
			env := b.ran(t, "env")
			for _, want := range []string{"HOMEBREW_NO_ASK=1", "HOMEBREW_AUTO_UPDATE_SECS=0", "HOMEBREW_NO_INSTALL_CLEANUP=1", "HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK=1"} {
				if !strings.Contains(env, want+"\n") {
					t.Errorf("brew ran without %s:\n%s", want, env)
				}
			}
			if strings.Contains(env, "GREEDY") {
				t.Errorf("brew ran greedy:\n%s", env)
			}
		})
	}
}

// npm is asked first whether its registry has the release, which it publishes after GitHub;
// then it installs exactly that version, finding node beside npm though a service's PATH
// has none. A prefix it may not write is a failed install.
func TestNpmUpgradesTermaInTheBackground(t *testing.T) {
	unixLayouts(t)
	for _, tc := range []struct {
		name, registry, install string
		want                    string
		offered, installed      bool
	}{
		{"published", "1.0.1", "upgrade", "1.0.1", true, true},
		{"not published yet", "", "upgrade", "", false, false},
		{"root-owned prefix", "1.0.1", "echo 'npm error code EACCES' >&2; exit 243", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// What a service's relay finds on PATH: no node.
			t.Setenv("PATH", "/usr/bin:/bin")
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			prefix := filepath.Join(root, "node")
			exe := filepath.Join(prefix, "lib", "node_modules", "@miradorlabs", "terma", "vendor", "terma")
			termaScript(t, exe, "1.0.0")
			script(t, filepath.Join(prefix, "bin", "node"), "exit 0")
			install := tc.install
			if install == "upgrade" {
				install = fmt.Sprintf("printf '#!/bin/sh\\necho \"terma 1.0.1\"\\n' > %s", exe)
			}
			ran := filepath.Join(root, "ran")
			script(t, filepath.Join(prefix, "bin", "npm"), fmt.Sprintf(`command -v node >/dev/null || { echo 'env: node: No such file or directory' >&2; exit 127; }
case "$1" in
view) [ -n '%[2]s' ] && echo '%[2]s' ;;
install) echo "$@" > %[1]s.args; %[3]s ;;
esac`, ran, tc.registry, install))
			m, ok := ManagedBy(exe)
			if !ok || len(m.Argv) == 0 {
				t.Fatalf("ManagedBy = %+v, %v", m, ok)
			}
			var out bytes.Buffer
			got, err := m.Upgrade(t.Context(), "1.0.1", &out)
			if got != tc.want || (err == nil) != (tc.want != "") || errors.Is(err, ErrNotOffered) == tc.offered {
				t.Fatalf("Upgrade = %q, %v; want %q, offered %v\n%s", got, err, tc.want, tc.offered, &out)
			}
			args, _ := os.ReadFile(ran + ".args")
			if want := "install --global --prefix " + prefix + " @miradorlabs/terma@1.0.1\n"; tc.installed != (string(args) == want) {
				t.Fatalf("npm install ran with %q; want it run %v, as %q", args, tc.installed, want)
			}
		})
	}
}

// A relay stopping while its package manager runs leaves it to finish by itself, since
// stopping it halfway can leave terma uninstalled; one that runs past its time is stopped.
func TestAnUpgradeOutlivesTheRelayButNotItsTime(t *testing.T) {
	b := newBrewed(t)
	done := filepath.Join(b.prefix, "done")
	b.brew(t, "sleep 1; touch "+done)
	m := b.manager(t)

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if _, err := m.Upgrade(ctx, "1.0.1", &bytes.Buffer{}); !errors.Is(err, context.Canceled) || time.Since(start) > 900*time.Millisecond {
		t.Fatalf("Upgrade = %v after %s, want it to stop waiting at once", err, time.Since(start))
	}
	if !eventually(func() bool { _, err := os.Stat(done); return err == nil }, 5*time.Second) {
		t.Fatal("the upgrade was stopped with the relay")
	}

	_ = os.Remove(done)
	ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := m.Upgrade(ctx, "1.0.1", &bytes.Buffer{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Upgrade = %v, want its time up", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(done); err == nil {
		t.Fatal("an upgrade past its time went on")
	}
}

func eventually(cond func() bool, within time.Duration) bool {
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}
