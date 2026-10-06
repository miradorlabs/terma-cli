package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// termaNotifyLine is the notify line an earlier terma's connect wrote into config.toml.
const termaNotifyLine = `notify = ["terma", "hook", "codex-notify"] # managed by terma (codex-notify adapter)`

// recordDisplaced records, as that connect did, the notifier it displaced in configPath.
func recordDisplaced(t *testing.T, dir, configPath string, previous []string) {
	t.Helper()
	if err := updateCodexNotifyRecord(dir, func(rec *codexNotifyRecord) { rec.Chains[configPath] = previous }); err != nil {
		t.Fatal(err)
	}
}

func TestRunPreviousCodexNotifyGuardsOnlyTermaArgv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	configDir := t.TempDir()
	cp, err := (exporter{dir: configDir}).ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	// A user program whose path merely contains "terma" and "codex-notify" is not refused as
	// a recursive chain.
	recordDisplaced(t, configDir, cp, []string{"/opt/terma-tools/codex-notify-desktop"})
	if err := runPreviousNotify(context.Background(), configDir, "{}"); err != nil && strings.Contains(err.Error(), "recursive") {
		t.Fatalf("legitimate notifier refused as recursive: %v", err)
	}
	recordDisplaced(t, configDir, cp, notifyCommand)
	if err := runPreviousNotify(context.Background(), configDir, "{}"); err == nil || !strings.Contains(err.Error(), "recursive") {
		t.Fatalf("terma's own argv should be refused as recursive, got %v", err)
	}
}

// Removing a notify line that displaced nothing gives back the file connect found, byte for byte.
func TestCodexNotifyRemoveRestoresTheFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	configDir := t.TempDir()
	path := filepath.Join(home, "config.toml")
	original := "model = \"gpt-5.4\"\n\n[otel]\nlog_user_prompt = false\n"
	connected := "model = \"gpt-5.4\"\n\n" + termaNotifyLine + "\n\n[otel]\nlog_user_prompt = false\n"
	if err := os.WriteFile(path, []byte(connected), 0o600); err != nil {
		t.Fatal(err)
	}
	c := exporter{dir: configDir}
	if st, err := c.notifySetting(); err != nil || !st.Terma {
		t.Fatalf("status should report terma's notify: %+v (%v)", st, err)
	}
	changed, err := c.removeNotify()
	if err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	if data, _ := os.ReadFile(path); string(data) != original {
		t.Fatalf("remove left:\n%s\nwant:\n%s", data, original)
	}
	if changed, err := c.removeNotify(); err != nil || changed {
		t.Fatalf("second remove should be a no-op: changed=%v err=%v", changed, err)
	}
}

// A developer's own notifier, even a multiline one, is not terma's, and removal leaves it.
func TestCodexNotifyRemoveLeavesAForeignNotifier(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	configDir := t.TempDir()
	path := filepath.Join(home, "config.toml")
	original := "notify = [\n  \"my-notifier\",\n  \"--flag\",\n]\n\n[otel]\nlog_user_prompt = false\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	c := exporter{dir: configDir}
	if st, err := c.notifySetting(); err != nil || st.Terma {
		t.Fatalf("multiline notify should read as a foreign program: %+v (%v)", st, err)
	}
	if changed, err := c.removeNotify(); err != nil || changed {
		t.Fatalf("remove touched a foreign notifier: changed=%v err=%v", changed, err)
	}
	if data, _ := os.ReadFile(path); string(data) != original {
		t.Fatalf("a foreign notifier was rewritten:\n%s", data)
	}
}

func TestCodexNotifyRestoresUserProgram(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	configDir := t.TempDir()
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte(termaNotifyLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recordDisplaced(t, configDir, path, []string{"my-notifier", "--flag"})
	c := exporter{dir: configDir}
	if changed, err := c.removeNotify(); err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "notify = [\"my-notifier\", \"--flag\"]\n" {
		t.Fatalf("previous notifier was not restored:\n%s", data)
	}
	if rec, _, err := loadCodexNotifyRecord(configDir); err != nil || len(rec.Chains) != 0 {
		t.Fatalf("the record outlived the restore: %+v (%v)", rec, err)
	}
	// A missing config file has no notify of terma's, and removal is a no-op.
	_ = os.Remove(path)
	if st, err := c.notifySetting(); err != nil || st.Terma {
		t.Fatalf("missing file: %+v %v", st, err)
	}
	if changed, err := c.removeNotify(); err != nil || changed {
		t.Fatalf("remove on missing file: changed=%v err=%v", changed, err)
	}
}

// Each Codex config gets its own displaced notifier back, and one that had none gets none.
func TestCodexNotifyKeepsOneChainPerConfig(t *testing.T) {
	configDir := t.TempDir()
	homeA, homeB, homeC := t.TempDir(), t.TempDir(), t.TempDir()
	for home, previous := range map[string]string{homeA: "notifier-A", homeB: "notifier-B", homeC: ""} {
		path := filepath.Join(home, "config.toml")
		if err := os.WriteFile(path, []byte(termaNotifyLine+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if previous != "" {
			recordDisplaced(t, configDir, path, []string{previous})
		}
	}
	for home, want := range map[string]string{homeA: "notifier-A", homeB: "notifier-B", homeC: ""} {
		t.Setenv("CODEX_HOME", home)
		if _, err := (exporter{dir: configDir}).removeNotify(); err != nil {
			t.Fatalf("remove under %s: %v", home, err)
		}
		data, _ := os.ReadFile(filepath.Join(home, "config.toml"))
		if strings.Contains(string(data), "codex-notify") || want != "" && !strings.Contains(string(data), want) || want == "" && strings.Contains(string(data), "notify") {
			t.Errorf("%s was not given its own notifier (%q) back:\n%s", home, want, data)
		}
	}
}
