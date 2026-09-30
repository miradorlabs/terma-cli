package harness

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func sandbox(t *testing.T) {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

// Disconnect puts back what each key held before terma, removes what terma added, and
// leaves alone a key someone has edited since.
func TestJournalRestoresOnlyWhatTermaStillOwns(t *testing.T) {
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
	sandbox(t)
	j := NewJournal("agent", "/cfg", nil, map[string]string{"A": "terma"}, nil, nil, nil)
	j.ProjectID = "p1"
	if err := j.Save(); err != nil {
		t.Fatal(err)
	}
	path, err := JournalPath("agent", "/cfg")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal file %v: %v", info, err)
	}
	got, err := LoadJournal("agent", "/cfg")
	if err != nil || got == nil || got.ProjectID != "p1" || got.Installed["A"] != "terma" {
		t.Fatalf("loaded %+v, %v", got, err)
	}
	if other, _ := LoadJournal("agent", "/other"); other != nil {
		t.Fatalf("another config's journal read back: %+v", other)
	}
	if err := DeleteJournal("agent", "/cfg"); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadJournal("agent", "/cfg"); got != nil {
		t.Fatalf("deleted journal read back: %+v", got)
	}
}

// A write through a symlink lands on the file it points at; a dangling link is refused
// rather than replaced by a regular file.
func TestResolveWritePath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	resolvedReal, _ := filepath.EvalSymlinks(target)
	if got, linked, err := ResolveWritePath(link); err != nil || !linked || got != resolvedReal {
		t.Fatalf("link: %q %v %v", got, linked, err)
	}
	dangling := filepath.Join(dir, "dangling.json")
	if err := os.Symlink(filepath.Join(dir, "gone.json"), dangling); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveWritePath(dangling); err == nil {
		t.Fatal("a dangling link was accepted")
	}
	missing := filepath.Join(dir, "missing.json")
	if got, linked, err := ResolveWritePath(missing); err != nil || linked || got != missing {
		t.Fatalf("missing: %q %v %v", got, linked, err)
	}
}

// A backup is private whatever the original's mode, and a second connect keeps the
// first backup unless asked to replace it.
func TestBackupFile(t *testing.T) {
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
	for raw, want := range map[string][]Signal{
		"":                     AllSignals,
		"none":                 {},
		"metrics, traces,logs": AllSignals,
		"logs,logs":            {SignalLogs},
	} {
		if got, err := ParseSignals(raw); err != nil || !slices.Equal(got, want) {
			t.Errorf("ParseSignals(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	if _, err := ParseSignals("trace"); err == nil {
		t.Error("a misspelt signal was accepted")
	}
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
	sandbox(t)
	dir, err := HelpersDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "agent-otel-p1")
	const key = "ter_srv_0123456789abcdef0123456789abcdef"
	if err := WriteHelper(path, key); err != nil {
		t.Fatal(err)
	}
	if got := KeyFromHelper(path); got != key {
		t.Fatalf("KeyFromHelper = %q", got)
	}
	if !IsOwnHelper(path) || IsOwnHelper("/usr/local/bin/their-helper") {
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

// Evidence is read only from a regular, bounded, well-formed file, and each field is
// copied only in the shape it is allowed.
func TestEvidence(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for path, want := range map[string]string{
		filepath.Join(dir, "missing.json"): "missing",
		dir:                                "unsupported",
		write("big.json", `{"a":"`+strings.Repeat("x", EvidenceFileLimit)+`"}`): "oversized",
		write("bad.json", `{not json`):                                          "malformed",
		write("good.json", `{"a":1}`):                                           "present",
	} {
		if _, got := ReadEvidenceJSON(path); got != want {
			t.Errorf("%s: status %q, want %q", filepath.Base(path), got, want)
		}
	}
	var doc map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"plan":"team_tier_1","bad":"has space","on":true,"n":3.5,"huge":1e12,"neg":-1}`), &doc)
	dst := map[string]any{}
	CopyEvidenceString(dst, doc, "plan", "plan")
	CopyEvidenceString(dst, doc, "bad", "bad")
	CopyEvidenceBool(dst, doc, "on", "on")
	CopyEvidenceNumber(dst, doc, "n", "n", 10, false)
	CopyEvidenceNumber(dst, doc, "n", "whole", 10, true)
	CopyEvidenceNumber(dst, doc, "huge", "huge", 10, false)
	CopyEvidenceNumber(dst, doc, "neg", "neg", 10, false)
	if want := map[string]any{"plan": "team_tier_1", "on": true, "n": 3.5}; !maps.Equal(dst, want) {
		t.Fatalf("copied %v, want %v", dst, want)
	}
	for s, want := range map[string]bool{"a@b.co": true, "a@b": false, "@b.co": false, "a b@c.de": false, `a"@b.co`: false} {
		if ValidEmail(s) != want {
			t.Errorf("ValidEmail(%q) = %v", s, !want)
		}
	}
	for s, want := range map[string]bool{"12.345678901234567890": true, "0": true, "": false, "1.2.3": false, "-1": false, "1e3": false} {
		if ValidCreditBalance(s) != want {
			t.Errorf("ValidCreditBalance(%q) = %v", s, !want)
		}
	}
}

type named struct{ Harness }

func (named) Name() string { return "agent" }

type servicenamed struct{ named }

func (servicenamed) ServiceName() string { return "agent-service" }

func TestServiceName(t *testing.T) {
	if got := ServiceName(named{}); got != "agent" {
		t.Errorf("default service name %q", got)
	}
	if got := ServiceName(servicenamed{}); got != "agent-service" {
		t.Errorf("declared service name %q", got)
	}
}
