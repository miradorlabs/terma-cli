package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// upgradeLog is a fresh log for one upgrade.
func upgradeLog(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "update", upgradeLogFile)
}

// Homebrew upgrades terma in the background with no terminal and no prompt, its taps updated
// first when asked, the developer's other packages left alone, its output in the upgrade's
// log; success is the version the link it moved now runs, never its exit status.
func TestHomebrewUpgradesTermaInTheBackground(t *testing.T) {
	notUpgrading := "echo 'Warning: Not upgrading terma, the latest version is already installed'"
	for _, tc := range []struct {
		name, to string
		refresh  bool
		then     func(b brewed) string
		want     string
		is       error
	}{
		{"upgraded", "1.0.1", true, func(b brewed) string { return b.upgradeTo("1.0.1") }, "1.0.1", nil},
		{"upgraded, then failed", "1.0.1", true, func(b brewed) string { return b.upgradeTo("1.0.1") + "; echo 'Error: completions' >&2; exit 1" }, "1.0.1", nil},
		// A tap a release behind still names one newer than what is installed.
		{"a tap a release behind", "1.0.2", true, func(b brewed) string { return b.upgradeTo("1.0.1") }, "1.0.1", nil},
		// Its tap was not updated: it says terma is up to date.
		{"tap without it yet", "1.0.1", false, func(brewed) string { return notUpgrading }, "", ErrNotOffered},
		{"busy", "1.0.1", true, func(brewed) string {
			return "echo 'Error: A `brew upgrade terma` process has already locked /opt/homebrew/var/homebrew/locks/terma.lock.' >&2; exit 1"
		}, "", ErrBusy},
		{"untrusted tap", "1.0.1", true, func(brewed) string { return "echo 'Error: Refusing to load untrusted tap miradorlabs/tap' >&2; exit 1" }, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBrewed(t)
			b.brew(t, tc.then(b))
			log := upgradeLog(t)
			got, err := b.manager(t).Upgrade(t.Context(), UpgradeRequest{From: "1.0.0", To: tc.to, Log: log, RefreshTaps: tc.refresh})
			switch {
			case tc.want != "" && (got != tc.want || err != nil):
				t.Fatalf("Upgrade = %q, %v; want %q installed", got, err, tc.want)
			case tc.want == "" && (err == nil || tc.is != nil && !errors.Is(err, tc.is) || tc.is == nil && retrySoon(err)):
				t.Fatalf("Upgrade = %q, %v; want it to fail as %v", got, err, tc.is)
			}
			if args := strings.TrimSpace(b.ran(t, "args")); args != "upgrade --cask terma" {
				t.Errorf("brew ran with %q", args)
			}
			if stdin := b.ran(t, "stdin"); stdin != "" {
				t.Errorf("brew's input was %q, want empty", stdin)
			}
			env := b.ran(t, "env")
			for _, want := range []string{"HOMEBREW_NO_ASK=1", "HOMEBREW_NO_INSTALL_CLEANUP=1", "HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK=1"} {
				if !strings.Contains(env, want+"\n") {
					t.Errorf("brew ran without %s:\n%s", want, env)
				}
			}
			if refreshed := strings.Contains(env, "HOMEBREW_AUTO_UPDATE_SECS=0\n"); refreshed != tc.refresh {
				t.Errorf("brew updated its taps first: %v, want %v", refreshed, tc.refresh)
			}
			if strings.Contains(env, "GREEDY") {
				t.Errorf("brew ran greedy:\n%s", env)
			}
			if data, _ := os.ReadFile(log); !strings.Contains(string(data), "upgrading 1.0.0 → "+tc.to+" with Homebrew") {
				t.Errorf("the upgrade's log reads %q", data)
			}
		})
	}
}

// npm is asked first whether its registry has the release, which it publishes after GitHub;
// then it installs exactly that version, finding node beside npm though a service's PATH
// has none, and running the install script that downloads the binary whatever
// ignore-scripts says. A prefix it may not write is a failed install.
func TestNpmUpgradesTermaInTheBackground(t *testing.T) {
	unixLayouts(t)
	for _, tc := range []struct {
		name, registry, install string
		ignoreScripts           bool
		want                    string
		offered, installed      bool
	}{
		{"published", "1.0.1", "upgrade", false, "1.0.1", true, true},
		// npm would install the package and skip the script that downloads the binary.
		{"scripts ignored by the developer's npm config", "1.0.1", "upgrade", true, "1.0.1", true, true},
		{"not published yet", "", "upgrade", false, "", false, false},
		{"root-owned prefix", "1.0.1", "echo 'npm error code EACCES' >&2; exit 243", false, "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// What a service's relay finds on PATH: no node.
			t.Setenv("PATH", "/usr/bin:/bin")
			if tc.ignoreScripts {
				t.Setenv("npm_config_ignore_scripts", "true")
			}
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
			// Like npm, it skips the package's install script when ignore-scripts is set and
			// the command line does not say otherwise: the package lands without its binary.
			script(t, filepath.Join(prefix, "bin", "npm"), fmt.Sprintf(`command -v node >/dev/null || { echo 'env: node: No such file or directory' >&2; exit 127; }
case "$1" in
view) [ -n '%[2]s' ] && echo '%[2]s' ;;
install) echo "$@" > %[1]s.args
  case " $* " in *" --ignore-scripts=false "*) ;; *) [ "$npm_config_ignore_scripts" = true ] && { rm -f %[4]s; exit 0; } ;; esac
  %[3]s ;;
esac`, ran, tc.registry, install, exe))
			m, ok := ManagedBy(exe)
			if !ok || len(m.Argv) == 0 {
				t.Fatalf("ManagedBy = %+v, %v", m, ok)
			}
			got, err := m.Upgrade(t.Context(), UpgradeRequest{From: "1.0.0", To: "1.0.1", Log: upgradeLog(t)})
			if got != tc.want || (err == nil) != (tc.want != "") || errors.Is(err, ErrNotOffered) == tc.offered {
				t.Fatalf("Upgrade = %q, %v; want %q, offered %v", got, err, tc.want, tc.offered)
			}
			args, _ := os.ReadFile(ran + ".args")
			if want := "install --global --prefix " + prefix + " --ignore-scripts=false @miradorlabs/terma@1.0.1\n"; tc.installed != (string(args) == want) {
				t.Fatalf("npm install ran with %q; want it run %v, as %q", args, tc.installed, want)
			}
		})
	}
}

// A relay stopping while its package manager runs leaves it to finish by itself, since
// stopping it halfway can leave terma uninstalled, and says what it installed should the
// release be in place already; one that runs past its time is stopped, with every process
// it started.
func TestAnUpgradeOutlivesTheRelayButNotItsTime(t *testing.T) {
	t.Parallel()
	b := newBrewed(t)
	done := filepath.Join(b.prefix, "done")
	b.brew(t, "sleep 4; touch "+done)
	m := b.manager(t)
	r := UpgradeRequest{From: "1.0.0", To: "1.0.1", Log: upgradeLog(t)}

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if _, err := m.Upgrade(ctx, r); !errors.Is(err, context.Canceled) || !errors.Is(err, errLeftRunning) || time.Since(start) > 3*time.Second {
		t.Fatalf("Upgrade = %v after %s, want it to stop waiting at once, brew left running", err, time.Since(start))
	}
	if !eventually(func() bool { _, err := os.Stat(done); return err == nil }, 10*time.Second) {
		t.Fatal("the upgrade was stopped with the relay")
	}

	// Stopped once the release is in place, as a hook of the new release asks the relay to.
	b.brew(t, b.upgradeTo("1.0.1")+"; sleep 2")
	ctx, cancel = context.WithCancel(t.Context())
	go func() {
		eventually(func() bool { dest, _ := os.Readlink(b.link); return strings.Contains(dest, "1.0.1") }, 10*time.Second)
		cancel()
	}()
	if got, err := m.Upgrade(ctx, r); got != "1.0.1" || err != nil {
		t.Fatalf("Upgrade stopped after the swap = %q, %v; want 1.0.1 installed", got, err)
	}

	b = newBrewed(t)
	left := filepath.Join(b.prefix, "left")
	b.brew(t, "(sleep 2; touch "+left+") & sleep 10")
	m = b.manager(t)
	// Long enough for macOS to let a script just written run.
	ctx, cancel = context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	if _, err := m.Upgrade(ctx, r); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Upgrade = %v, want its time up", err)
	}
	if b.ran(t, "args") == "" {
		t.Fatal("brew had not started by its deadline")
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(left); err == nil {
		t.Fatal("what an upgrade past its time started went on")
	}
}

// A relay told to stop starts no package manager, nor anything else RunDetached would run:
// it would outlive the relay, or be stopped with it halfway.
func TestAStoppingRelayStartsNoPackageManager(t *testing.T) {
	t.Parallel()
	b := newBrewed(t)
	started := filepath.Join(b.prefix, "started")
	b.brew(t, "touch "+started)
	ctx, stop := context.WithCancel(t.Context())
	stop()
	if _, err := b.manager(t).Upgrade(ctx, UpgradeRequest{From: "1.0.0", To: "1.0.1", Log: upgradeLog(t)}); !errors.Is(err, context.Canceled) || errors.Is(err, errLeftRunning) {
		t.Fatalf("Upgrade = %v, want it refused, nothing left running", err)
	}
	if err := RunDetached(ctx, nil, os.Environ(), filepath.Join(b.prefix, "bin", "brew")); !errors.Is(err, context.Canceled) || errors.Is(err, errLeftRunning) {
		t.Fatalf("RunDetached = %v, want it refused, nothing left running", err)
	}
	if eventually(func() bool { _, err := os.Stat(started); return err == nil }, 2*time.Second) {
		t.Fatal("brew started for a relay told to stop")
	}
}

// A relay stopped while it asks npm's registry, before npm installs anything, leaves nothing
// running: what lands later is not its doing.
func TestAStopDuringNpmViewLeavesNothingRunning(t *testing.T) {
	unixLayouts(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(root, "node")
	exe := filepath.Join(prefix, "lib", "node_modules", "@miradorlabs", "terma", "vendor", "terma")
	termaScript(t, exe, "1.0.0")
	installed := filepath.Join(root, "installed")
	script(t, filepath.Join(prefix, "bin", "npm"), `case "$1" in view) sleep 5; echo 1.0.1 ;; install) touch `+installed+` ;; esac`)
	m, _ := ManagedBy(exe)
	ctx, stop := context.WithCancel(t.Context())
	time.AfterFunc(300*time.Millisecond, stop)
	if _, err := m.Upgrade(ctx, UpgradeRequest{From: "1.0.0", To: "1.0.1", Log: upgradeLog(t)}); err == nil || errors.Is(err, errLeftRunning) {
		t.Fatalf("Upgrade = %v, want it stopped with nothing left running", err)
	}
	if _, err := os.Stat(installed); err == nil {
		t.Fatal("npm install ran for a relay told to stop")
	}
}

// The probe of systemd-run runs to its end though the relay is told to stop meanwhile: cut
// short, it would read as no scope, and the package manager would run in the relay's unit.
func TestTheScopeProbeOutlivesAStoppingRelay(t *testing.T) {
	t.Parallel()
	unixLayouts(t)
	dir := t.TempDir()
	works, fails := filepath.Join(dir, "works"), filepath.Join(dir, "fails")
	script(t, works, "sleep 0.2; exit 0")
	script(t, fails, "exit 1")
	ctx, stop := context.WithCancel(t.Context())
	stop()
	if !scopeWorks(ctx, os.Environ())(works) {
		t.Fatal("a probe that succeeds read as failed once the relay was told to stop")
	}
	if scopeWorks(ctx, os.Environ())(fails) {
		t.Fatal("a probe that fails read as succeeded")
	}
}

// Busy is read from this upgrade's own output: an earlier upgrade left to finish goes on
// writing to its own file, now the previous log, never into this one's.
func TestAnUpgradesLogIsItsOwn(t *testing.T) {
	t.Parallel()
	path := upgradeLog(t)
	earlier := openLog(path)
	if earlier == nil {
		t.Fatal("no log")
	}
	defer func() { _ = earlier.Close() }()
	this := openLog(path)
	if this == nil {
		t.Fatal("no second log")
	}
	defer func() { _ = this.Close() }()
	if _, err := earlier.WriteString("Error: A `brew upgrade` process has already locked it.\n"); err != nil {
		t.Fatal(err)
	}
	if busy(this) || !busy(earlier) {
		t.Fatalf("busy: this %v, earlier %v; want only the earlier", busy(this), busy(earlier))
	}
	if data, _ := os.ReadFile(path + ".1"); !strings.Contains(string(data), "has already locked") {
		t.Fatalf("the previous log reads %q", data)
	}
}

// Under systemd the package manager runs in a scope of its own, which the relay's unit
// stopping does not stop; anywhere else, or where the user's systemd makes none, in a
// session of its own alone.
func TestScopedLeavesTheRelaysSystemdUnit(t *testing.T) {
	t.Parallel()
	argv := []string{"/opt/homebrew/bin/brew", "upgrade", "--cask", "terma"}
	scope := []string{"/usr/bin/systemd-run", "--user", "--scope", "--quiet", "--collect", "--"}
	for _, tc := range []struct {
		name, goos, invocation string
		found, works           bool
		want                   []string
	}{
		{"systemd unit", "linux", "0123abcd", true, true, append(slices.Clone(scope), argv...)},
		{"no systemd unit", "linux", "", true, true, argv},
		{"no systemd-run", "linux", "0123abcd", false, true, argv},
		{"no user systemd", "linux", "0123abcd", true, false, argv},
		{"macOS", "darwin", "0123abcd", true, true, argv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			got := scoped(tc.goos, func(k string) string {
				if k == "INVOCATION_ID" {
					return tc.invocation
				}
				return ""
			}, func(string) (string, error) {
				if !tc.found {
					return "", errors.New("not found")
				}
				return scope[0], nil
			}, func(argv ...string) bool {
				asked = argv
				return tc.works
			}, argv)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("scoped = %q, want %q", got, tc.want)
			}
			if asked != nil && !slices.Equal(asked, append(slices.Clone(scope), "true")) {
				t.Fatalf("asked systemd-run %q", asked)
			}
		})
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
