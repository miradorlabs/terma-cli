package relay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLaunchdPlistEscapesAndRoundTrips(t *testing.T) {
	binary := `/Users/a&b/<bin>/terma`
	plist := string(launchdPlist(binary, [][2]string{{"TERMA_CONFIG_DIR", "/tmp/x&y"}}))
	for _, want := range []string{"<string>/Users/a&amp;b/&lt;bin&gt;/terma</string>", "<string>relay</string>", "<string>serve</string>",
		"<key>KeepAlive</key>\n\t<true/>", "<key>TERMA_CONFIG_DIR</key>\n\t\t<string>/tmp/x&amp;y</string>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	m := plistBinaryRe.FindStringSubmatch(plist)
	if m == nil || xmlUnescape(m[1]) != binary {
		t.Fatalf("binary read back as %v", m)
	}
}

func TestSystemdUnitQuotesAndRoundTrips(t *testing.T) {
	binary := `/home/a b/$HOME/100%/"terma"`
	unit := string(systemdUnitFile(binary, [][2]string{{"CODEX_HOME", "/home/a b/.codex"}}))
	if !strings.Contains(unit, "Restart=always") || !strings.Contains(unit, `Environment="CODEX_HOME=/home/a b/.codex"`) {
		t.Fatalf("unit:\n%s", unit)
	}
	m := unitExecRe.FindStringSubmatch(unit)
	if m == nil || systemdUnquote(m[1]) != binary {
		t.Fatalf("binary read back as %v from\n%s", m, unit)
	}
}

func TestInstallServiceIsIdempotent(t *testing.T) {
	if !Supported() {
		t.Skip("no service manager")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(home, "terma"))
	running, registered := false, false
	var calls []string
	orig := runCommand
	runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch {
		case name == "launchctl" && args[0] == "print" && running:
			return []byte("\tstate = running\n\tpid = 42\n"), nil
		case name == "launchctl" && args[0] == "print":
			return nil, os.ErrNotExist
		case name == "launchctl" && args[0] == "bootstrap", name == "systemctl" && args[1] == "restart":
			running = true
		case name == "systemctl" && args[1] == "is-active" && running:
			return []byte("active\n"), nil
		case name == "systemctl" && args[1] == "is-active":
			return []byte("inactive\n"), nil
		// Windows: the Run key, the wscript launcher, the supervisor's process.
		case name == "reg" && args[0] == "add":
			registered = true
		case name == "reg" && args[0] == "delete":
			registered = false
		case name == "reg" && args[0] == "query" && !registered:
			return nil, os.ErrNotExist
		case name == "wscript.exe":
			running = true
			_ = RecordSupervisor()
		case name == "taskkill":
			running = false
		case name == "tasklist" && running:
			return []byte(fmt.Sprintf("terma.exe  %d Console\n", os.Getpid())), nil
		}
		return nil, nil
	}
	t.Cleanup(func() { runCommand = orig })

	ctx := context.Background()
	if err := InstallService(ctx, "/opt/terma/bin/terma"); err != nil {
		t.Fatal(err)
	}
	st, err := Service(ctx)
	if err != nil || !st.Installed || !st.Running || st.Binary != "/opt/terma/bin/terma" {
		t.Fatalf("after install: %+v, %v", st, err)
	}
	before := len(calls)
	if err := InstallService(ctx, "/opt/terma/bin/terma"); err != nil {
		t.Fatal(err)
	}
	for _, c := range calls[before:] {
		if strings.Contains(c, "bootstrap") || strings.Contains(c, "restart") {
			t.Fatalf("an unchanged, running service was restarted: %v", calls[before:])
		}
		if strings.Contains(c, "wscript") || strings.Contains(c, "taskkill") {
			t.Fatalf("an unchanged, running service was restarted: %v", calls[before:])
		}
	}
	if err := UninstallService(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := Service(ctx); st.Installed {
		t.Fatal("still installed after uninstall")
	}
}

func TestSweepBoundsTheQueue(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-maxQueueAge - time.Hour)
	out := filepath.Join(dir, outboxDir, "proj")
	e1 := newEntry(old, sigLogs, formatJSON)
	e2 := newEntry(now, sigLogs, formatJSON)
	for _, e := range []entry{e1, e2} {
		if err := writeEntry(out, e, []byte("{}")); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Chtimes(filepath.Join(out, e1.name), old, old)
	if dropped := sweep(dir, now); dropped != 1 {
		t.Fatalf("dropped %d, want the expired body only", dropped)
	}
	if left, _ := listEntries(out); len(left) != 1 || left[0].name != e2.name {
		t.Fatalf("left %v", left)
	}
}

func TestEntryNamesRoundTrip(t *testing.T) {
	e := newEntry(time.Unix(1790695797, 123), sigMetrics, formatProto)
	got, ok := parseEntry(e.name)
	if !ok || got.sig != sigMetrics || got.format != formatProto || !got.received.Equal(e.received) {
		t.Fatalf("parsed %+v from %s", got, e.name)
	}
	for _, bad := range []string{".tmp-123", "x.json", "1-2-bogus.json", "1-2-logs.txt"} {
		if _, ok := parseEntry(bad); ok {
			t.Errorf("%s parsed", bad)
		}
	}
}

func TestRecordSessionNeedsASetUpRelay(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", cfg)
	if err := RecordSession(sessionA, repoPath("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg, dirName)); !os.IsNotExist(err) {
		t.Fatal("a hook created the relay's directory on a machine without a relay")
	}
	if _, err := Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := RecordSession(sessionA, repoPath("a")); err != nil {
		t.Fatal(err)
	}
	if got := sessionDir(filepath.Join(cfg, dirName), sessionA); got != repoPath("a") {
		t.Fatalf("recorded %q", got)
	}
	if err := RecordSession("../escape", "/x"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg, dirName, "escape")); !os.IsNotExist(err) {
		t.Fatal("an unsafe id named a file")
	}
}

func TestEnsureKeepsAnExistingToken(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	first, err := Ensure()
	if err != nil || first.Port != DefaultPort || len(first.Token) != 64 {
		t.Fatalf("%+v %v", first, err)
	}
	second, err := Ensure()
	if err != nil || second != first {
		t.Fatalf("Ensure rotated the token: %+v", second)
	}
}
