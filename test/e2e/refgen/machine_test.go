package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
)

// machineProfiles are a night of a build whose observed values name the machine everywhere they
// can (a files profile's commands and paths, a desktop launch's variables, a hook's payload, the
// binary's facts) and whose vendor text names paths as examples, as real builds do.
func machineProfiles(version string) []e2e.Profile {
	first := claudeProfile(version, "Work in /home/user/monorepo/CLAUDE.md like /Users/name/My Documents, never file:///Users/name/x.", []string{"Bash"}, "", "")
	first.Probes = map[string]json.RawMessage{
		"launch": json.RawMessage(`{"argv":["<agent>","--model=x"],"env":["CLAUDE_CODE_DIAGNOSTICS_FILE","CODEX_TECTONIC_PATH","HOMEBREW_PREFIX"],
			"env_values":{"CLAUDE_CODE_DIAGNOSTICS_FILE":"/Users/<user>/Library/Logs/Claude-3p/cli-diagnostics.jsonl",
			"CODEX_TECTONIC_PATH":"/Users/<user>/work/terma-cli/terma-cli/test/e2e/versions/codex-desktop/<version>/ChatGPT.app/Contents/Resources/tectonic/tectonic",
			"HOMEBREW_PREFIX":"/opt/homebrew", "DATA":"file:///System/Volumes/Data/Users/runner/x"}}`),
	}
	first.Binary = json.RawMessage(`{"form":"executable","size_bytes":1,"agent":{"path":"/Users/runner/.local/bin/claude"}}`)
	files := filesRow(version, "darwin/arm64", map[string]any{
		"/Applications/Xcode_26.6.app/Contents/Developer/usr/share/git-core/templates": map[string]any{"ops": []string{"read"}, "phases": []string{"startup"}},
		// A build's own path that looks like a home: Claude Code's remote token.
		"/home/claude/.claude/remote/.oauth_token": map[string]any{"ops": []string{"probe"}, "phases": []string{"startup"}},
	})
	var probe map[string]any
	_ = json.Unmarshal(files.Probes["filesystem"], &probe)
	probe["exec"] = map[string]any{
		"/opt/homebrew/bin/git config user.name": map[string]any{"argv": []string{"/opt/homebrew/bin/git", "config", "user.name"}, "phases": []string{"startup"},
			"count": 2, "by": "claude", "parents": []string{}, "role": "harness"},
		"git config user.name": map[string]any{"argv": []string{"git", "config", "user.name"}, "phases": []string{"turn"},
			"count": 1, "by": "zsh", "parents": []string{"/bin/zsh -c git config user.name"}, "role": "tool"},
		"/Applications/Xcode_26.6.app/Contents/Developer/usr/bin/xcodebuild -license check": map[string]any{
			"argv":  []string{"/Applications/Xcode_26.6.app/Contents/Developer/usr/bin/xcodebuild", "-license", "check"},
			"count": 1, "by": "claude", "parents": []string{"/opt/homebrew/bin/git config user.name"}, "role": "harness", "phases": []string{"startup"}},
	}
	files.Probes["filesystem"], _ = json.Marshal(probe)
	return []e2e.Profile{first, files}
}

// The publish-time guard: a night whose observed values name the machine publishes none of it,
// and a vendor's own text keeps the paths it names as examples.
func TestPublishNamesNoMachinePaths(t *testing.T) {
	dir := t.TempDir()
	for i, v := range []string{"1.0.0", "1.0.1"} {
		n := night{now: time.Date(2026, 11, 3+i, 5, 0, 0, 0, time.UTC), profiles: machineProfiles(v)}
		if _, err := publish(dir, n, options{}); err != nil {
			t.Fatal(err)
		}
	}
	leaks := scanMachineLeaks(t, dir)
	if len(leaks.samples) > 0 {
		t.Errorf("published observed values name the machine (%d):\n%s", leaks.total(), strings.Join(leaks.samples, "\n"))
	}
	var s Snapshot
	if err := readJSON(filepath.Join(dir, "claude", "versions", "1.0.1.json"), &s); err != nil {
		t.Fatal(err)
	}
	exec := s.Filesystem["darwin/arm64"].(map[string]any)["exec"].(map[string]any)
	// Homebrew's git and git by its name are one command: their runs merged.
	git, _ := exec["git config user.name"].(map[string]any)
	if git["count"] != float64(3) || git["role"] != e2e.RoleBoth || !reflect.DeepEqual(git["phases"], []any{"startup", "turn"}) {
		t.Errorf("git config user.name = %v (%v)", git, slices.Sorted(keysOf(exec)))
	}
	if x, _ := exec["<xcode>/usr/bin/xcodebuild -license check"].(map[string]any); x == nil || !reflect.DeepEqual(x["parents"], []any{"git config user.name"}) {
		t.Errorf("xcodebuild = %v (%v)", x, slices.Sorted(keysOf(exec)))
	}
	paths := s.Filesystem["darwin/arm64"].(map[string]any)["paths"].(map[string]any)
	if paths["<xcode>/usr/share/git-core/templates"] == nil || paths["/home/claude/.claude/remote/.oauth_token"] == nil {
		t.Errorf("paths = %v", slices.Sorted(keysOf(paths)))
	}
	env := s.CLI["launch"].(map[string]any)["env_values"].(map[string]any)
	for k, want := range map[string]string{"CLAUDE_CODE_DIAGNOSTICS_FILE": "<home>/Library/Logs/Claude-3p/cli-diagnostics.jsonl",
		"CODEX_TECTONIC_PATH": "<app>/Contents/Resources/tectonic/tectonic", "HOMEBREW_PREFIX": "<machine>", "DATA": "file://<home>/x"} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if agent := s.Binary["agent"].(map[string]any); agent["path"] != "<home>/.local/bin/claude" {
		t.Errorf("binary.agent.path = %v", agent["path"])
	}
	// The vendor's text is the build's: its example paths stay.
	md, _ := os.ReadFile(filepath.Join(dir, "claude", "prompts", "1.0.1.md"))
	if !strings.Contains(string(md), "/home/user/monorepo/CLAUDE.md like /Users/name/My Documents, never file:///Users/name/x.") {
		t.Errorf("the prompt's own example paths were changed:\n%s", md)
	}
}

// What was published before a rule named the machine's paths is named when refgen loads it
// back, and the diffs on either side of it are made again.
func TestLoadNamesPublishedMachinePaths(t *testing.T) {
	dir := t.TempDir()
	for i, v := range []string{"1.0.0", "1.0.1"} {
		n := night{now: time.Date(2026, 11, 3+i, 5, 0, 0, 0, time.UTC), profiles: []e2e.Profile{filesRow(v, "darwin/arm64", nil)}}
		if _, err := publish(dir, n, options{}); err != nil {
			t.Fatal(err)
		}
	}
	// As published before the rule: Homebrew's git by its path, in the snapshot and its diff.
	path := filepath.Join(dir, "claude", "versions", "1.0.1.json")
	var s Snapshot
	if err := readJSON(path, &s); err != nil {
		t.Fatal(err)
	}
	fs := s.Filesystem["darwin/arm64"].(map[string]any)
	exec := fs["exec"].(map[string]any)
	exec["/opt/homebrew/bin/git log -1"] = map[string]any{"argv": []any{"/opt/homebrew/bin/git", "log", "-1"}, "phases": []any{"startup"},
		"count": 1, "by": "claude", "parents": []any{}, "role": "harness"}
	if err := writeJSON(path, &s); err != nil {
		t.Fatal(err)
	}
	diffPath := filepath.Join(dir, "claude", "diffs", "1.0.0..1.0.1.json")
	var stale Diff
	if err := readJSON(diffPath, &stale); err != nil {
		t.Fatal(err)
	}
	stale.Changes = append(stale.Changes, Change{ID: "filesystem.darwin/arm64.exec./opt/homebrew/bin/git log -1", Category: "filesystem",
		Op: "added", Path: []string{"filesystem", "darwin/arm64", "exec", "/opt/homebrew/bin/git log -1"}, Title: "New command at startup: `/opt/homebrew/bin/git log -1`"})
	if err := writeJSON(diffPath, &stale); err != nil {
		t.Fatal(err)
	}
	if leaks := scanMachineLeaks(t, dir); leaks.total() == 0 {
		t.Fatal("the planted machine path is not seen")
	}
	if _, err := publish(dir, night{now: time.Date(2026, 11, 6, 5, 0, 0, 0, time.UTC)}, options{}); err != nil {
		t.Fatal(err)
	}
	if leaks := scanMachineLeaks(t, dir); len(leaks.samples) > 0 {
		t.Errorf("republished data still names the machine:\n%s", strings.Join(leaks.samples, "\n"))
	}
	var d Diff
	if err := readJSON(diffPath, &d); err != nil {
		t.Fatal(err)
	}
	if changeByID(&d, "filesystem.darwin/arm64.exec.git log -1") == nil {
		t.Errorf("the diff was not made again: %v", changeIDs(&d))
	}
	// Loading names the paths; it is no capture: when each section was captured stays.
	var after Snapshot
	if err := readJSON(path, &after); err != nil {
		t.Fatal(err)
	}
	if !after.CapturedAt.Equal(s.CapturedAt) || !reflect.DeepEqual(after.Capture.Sections, s.Capture.Sections) {
		t.Errorf("capture times moved: %v %v", after.CapturedAt, after.Capture.Sections)
	}
}

// TestPublishedMachineLeaks scans a published tree with the guard's rules: REFGEN_SCAN_DIR names
// it (docs/harnesses of a compat-matrix checkout, or a re-render of one). It counts what each
// observed value still names of the machine, by field and rule, and fails where anything does.
func TestPublishedMachineLeaks(t *testing.T) {
	dir := os.Getenv("REFGEN_SCAN_DIR")
	if dir == "" {
		t.Skip("REFGEN_SCAN_DIR names a published tree to scan")
	}
	leaks := scanMachineLeaks(t, dir)
	keys := make([]string, 0, len(leaks.counts))
	for k := range leaks.counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%6d  %s", leaks.counts[k], k)
	}
	t.Logf("%6d  observed values naming the machine in %s", leaks.total(), dir)
	if leaks.total() > 0 {
		t.Errorf("published observed values name the machine; first of them:\n%s", strings.Join(leaks.samples[:min(len(leaks.samples), 10)], "\n"))
	}
}

// machineScan is what a scan of a published tree found.
type machineScan struct {
	counts  map[string]int // "<file kind> <field> <rule>" → values
	samples []string
}

func (m machineScan) total() int {
	n := 0
	for _, c := range m.counts {
		n += c
	}
	return n
}

// scanMachineLeaks looks through every snapshot and diff under dir for observed values that
// still name the machine: a snapshot's machineFields, and a diff's changes to them.
func scanMachineLeaks(t *testing.T, dir string) machineScan {
	t.Helper()
	m := machineScan{counts: map[string]int{}}
	see := func(kind, field, file, s string) {
		if l := e2e.MachineLeak(s); l != "" {
			m.counts[kind+" "+field+" "+l]++
			if len(m.samples) < 200 {
				m.samples = append(m.samples, file+": "+field+": "+truncate(s, 160))
			}
		}
	}
	snaps, _ := filepath.Glob(filepath.Join(dir, "*", "versions", "*.json"))
	for _, f := range snaps {
		var s Snapshot
		if err := readJSON(f, &s); err != nil {
			t.Fatal(err)
		}
		for _, l := range machineLeaks(&s) {
			see("snapshot", l.field, f, l.value)
		}
	}
	diffs, _ := filepath.Glob(filepath.Join(dir, "*", "diffs", "*.json"))
	for _, f := range diffs {
		var d Diff
		if err := readJSON(f, &d); err != nil {
			t.Fatal(err)
		}
		for _, l := range diffMachineLeaks(&d) {
			see("diff", l.field, f, l.value)
		}
	}
	return m
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func changeIDs(d *Diff) []string {
	out := make([]string, 0, len(d.Changes))
	for _, c := range d.Changes {
		out = append(out, c.ID)
	}
	return out
}

// A run's id normalization once missed in an observed value is a placeholder when it is loaded
// back: Claude Desktop's local_<uuid> session id.
func TestScrubMachineRunIDs(t *testing.T) {
	s := &Snapshot{CLI: map[string]any{"launch": map[string]any{"env_values": map[string]any{
		"CLAUDE_CODE_HOST_SESSION_ID": "local_88d385d8-fbba-4414-a316-90943ceface5"}}}}
	if !scrubMachine(s) {
		t.Fatal("nothing named")
	}
	if got := s.CLI["launch"].(map[string]any)["env_values"].(map[string]any)["CLAUDE_CODE_HOST_SESSION_ID"]; got != "local_<uuid>" {
		t.Errorf("session id = %v", got)
	}
}

// The guard is publish's own: a snapshot whose observed value still names the machine (a path
// no rule names yet) makes publish refuse, naming the file, the field and the path, and write
// nothing.
func TestPublishRefusesMachinePaths(t *testing.T) {
	dir := t.TempDir()
	if _, err := publish(dir, night{now: time.Date(2026, 11, 3, 5, 0, 0, 0, time.UTC), profiles: []e2e.Profile{filesRow("1.0.0", "darwin/arm64", nil)}}, options{}); err != nil {
		t.Fatal(err)
	}
	before := treeBytes(t, dir)
	// A leak scrubbing leaves: what a build ran, after a placeholder no rule reads past.
	p := filesRow("1.0.1", "darwin/arm64", map[string]any{"<sandbox>/Users/runner/x": map[string]any{"ops": []string{"read"}, "phases": []string{"startup"}}})
	saved := scrubMachine
	scrubMachine = func(*Snapshot) bool { return false } // a rule missing for it
	defer func() { scrubMachine = saved }()
	_, err := publish(dir, night{now: time.Date(2026, 11, 4, 5, 0, 0, 0, time.UTC), profiles: []e2e.Profile{p}}, options{})
	if err == nil {
		t.Fatal("published a path that names the machine")
	}
	for _, want := range []string{"claude/versions/1.0.1.json", "filesystem", "holds /Users/"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q: %v", want, err)
		}
	}
	// The value stays out of it: a publish job's log is public.
	if strings.Contains(err.Error(), "runner/x") {
		t.Errorf("the refusal repeats the value: %v", err)
	}
	if after := treeBytes(t, dir); !reflect.DeepEqual(before, after) {
		t.Error("publish wrote files before it refused")
	}
}

// treeBytes is every file under dir, by path.
func treeBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		out[strings.TrimPrefix(path, dir)] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
