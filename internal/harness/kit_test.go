package harness

import (
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// Disconnect puts back what each key held before terma, removes what terma added, and
// leaves alone a key someone has edited since.
func TestJournalRestoresOnlyWhatTermaStillOwns(t *testing.T) {
	t.Parallel()
	existing := map[string]string{"A": "theirs", "EDITED": "before"}
	installing := map[string]string{"A": "terma", "B": "terma", "EDITED": "terma"}
	j := NewJournal("agent", "/cfg", existing, installing, map[string]string{"C": "cleared"}, nil, nil)

	env := map[string]string{"A": "terma", "B": "terma", "EDITED": "changed since"}
	res, left := j.Apply(env)
	if want := map[string]string{"A": "theirs", "EDITED": "changed since", "C": "cleared"}; !maps.Equal(env, want) {
		t.Fatalf("env after disconnect = %v, want %v", env, want)
	}
	if res.Restored != 2 || res.Removed != 1 || !slices.Equal(res.Skipped, []string{"EDITED"}) {
		t.Fatalf("result = %+v", res)
	}
	if left.Empty() || left.Installed["EDITED"] != "terma" {
		t.Fatalf("the edited key's record must survive: %+v", left)
	}
}

// A reconnect keeps the value from before terma, so the eventual disconnect restores
// that and not an intermediate terma value; an edit made since is what gets restored.
func TestJournalCarriesThePreTermaValueAcrossReconnects(t *testing.T) {
	t.Parallel()
	first := NewJournal("agent", "/cfg", map[string]string{"A": "theirs", "B": "theirs"},
		map[string]string{"A": "terma-1", "B": "terma-1"}, nil, nil, nil)
	second := NewJournal("agent", "/cfg", map[string]string{"A": "terma-1", "B": "edited"},
		map[string]string{"A": "terma-2", "B": "terma-2"}, nil, nil, first)
	env := map[string]string{"A": "terma-2", "B": "terma-2"}
	second.Apply(env)
	if env["A"] != "theirs" || env["B"] != "edited" {
		t.Fatalf("env = %v", env)
	}
}

func TestJournalSaveLoadDelete(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j := NewJournal("agent", "/cfg", nil, map[string]string{"A": "terma"}, nil, nil, nil)
	j.ProjectID = "p1"
	if err := j.Save(dir); err != nil {
		t.Fatal(err)
	}
	path := JournalPath(dir, "agent", "/cfg")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal file %v: %v", info, err)
	}
	got, err := LoadJournal(dir, "agent", "/cfg")
	if err != nil || got == nil || got.ProjectID != "p1" || got.Installed["A"] != "terma" {
		t.Fatalf("loaded %+v, %v", got, err)
	}
	if other, _ := LoadJournal(dir, "agent", "/other"); other != nil {
		t.Fatalf("another config's journal read back: %+v", other)
	}
	if err := DeleteJournal(dir, "agent", "/cfg"); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadJournal(dir, "agent", "/cfg"); got != nil {
		t.Fatalf("deleted journal read back: %+v", got)
	}
}

// A backup is private whatever the original's mode, and a second connect keeps the
// first backup unless asked to replace it.
func TestBackupFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "settings.json")
	if got, err := BackupFile(path, false, false); err != nil || got != "" {
		t.Fatalf("absent file: %q %v", got, err)
	}
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	backup, err := BackupFile(path, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(backup); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v", info.Mode().Perm())
	}
	_ = os.WriteFile(path, []byte("second"), 0o644)
	if _, err := BackupFile(path, true, false); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(backup); string(data) != "first" {
		t.Fatalf("an existing backup was overwritten: %q", data)
	}
	if _, err := BackupFile(path, true, true); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(backup); string(data) != "second" {
		t.Fatalf("replace kept the old backup: %q", data)
	}
}

func TestSignals(t *testing.T) {
	t.Parallel()
	if got := SignalsFromNames([]string{" LOGS", "nonsense", "traces"}); !slices.Equal(got, []Signal{SignalTraces, SignalLogs}) {
		t.Errorf("SignalsFromNames = %v", got)
	}
	if got := SignalNames(AllSignals); !slices.Equal(got, []string{"traces", "logs", "metrics"}) {
		t.Errorf("SignalNames = %v", got)
	}
	if got := WithoutSignal(slices.Clone(AllSignals), SignalLogs); !slices.Equal(got, []Signal{SignalTraces, SignalMetrics}) {
		t.Errorf("WithoutSignal = %v", got)
	}
}

// The helper script carries the key and nothing a shell would interpret; a key that
// could break out of it is refused.
func TestHeadersHelper(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(HelpersDir(dir), "agent-otel-p1")
	const key = "ter_srv_0123456789abcdef0123456789abcdef"
	if err := WriteHelper(path, key); err != nil {
		t.Fatal(err)
	}
	if got := KeyFromHelper(path); got != key {
		t.Fatalf("KeyFromHelper = %q", got)
	}
	if !IsOwnHelper(dir, path) || IsOwnHelper(dir, "/usr/local/bin/their-helper") {
		t.Fatal("IsOwnHelper told terma's script from another apart wrongly")
	}
	if err := WriteHelper(path, "ter_srv_$(rm -rf ~)"); err == nil {
		t.Fatal("a key a shell would interpret was written")
	}
	if err := DeleteHelper(path); err != nil {
		t.Fatal(err)
	}
	if err := DeleteHelper(path); err != nil || KeyFromHelper(path) != "" {
		t.Fatalf("a second delete: %v", err)
	}
}
