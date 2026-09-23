package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexNotifyBareInstallClearsStaleRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	path := filepath.Join(home, "config.toml")
	c := Codex{}
	if err := os.WriteFile(path, []byte("notify = [\"notifier-A\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InstallCodexNotify(); err != nil {
		t.Fatalf("first install: %v", err)
	}
	// The user strips terma's notify by hand, leaving no notifier behind.
	if err := os.WriteFile(path, []byte("model = \"gpt-5.4\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InstallCodexNotify(); err != nil {
		t.Fatalf("reinstall over no notifier: %v", err)
	}
	if _, err := c.RemoveCodexNotify(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "notifier-A") {
		t.Fatalf("stale notifier A resurrected on disconnect:\n%s", data)
	}
}

func TestRunPreviousCodexNotifyGuardsOnlyTermaArgv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	cp, err := (Codex{}).ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	// A user program whose path merely contains "terma" and "codex-notify" is not
	// terma's own notifier, so it must not be refused as a recursive chain.
	if err := saveCodexNotifyChain(cp, []string{"/opt/terma-tools/codex-notify-desktop"}); err != nil {
		t.Fatal(err)
	}
	if err := RunPreviousCodexNotify(context.Background(), "{}"); err != nil && strings.Contains(err.Error(), "recursive") {
		t.Fatalf("legitimate notifier refused as recursive: %v", err)
	}
	// terma's actual argv must still be refused.
	if err := saveCodexNotifyChain(cp, CodexNotifyCommand); err != nil {
		t.Fatal(err)
	}
	if err := RunPreviousCodexNotify(context.Background(), "{}"); err == nil || !strings.Contains(err.Error(), "recursive") {
		t.Fatalf("terma's own argv should be refused as recursive, got %v", err)
	}
}

func TestCodexNotifyInstallAndRemove(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte("model = \"gpt-5.4\"\n\n[otel]\nlog_user_prompt = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Codex{}
	st, err := c.CodexNotify()
	if err != nil || st.Configured {
		t.Fatalf("unexpected status %+v (%v)", st, err)
	}
	changed, err := c.InstallCodexNotify()
	if err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	if !strings.HasPrefix(got, "model = \"gpt-5.4\"\n") {
		t.Fatalf("existing top-level key disturbed:\n%s", got)
	}
	if !strings.Contains(got, `notify = ["terma", "hook", "codex-notify"] # managed by terma`) {
		t.Fatalf("notify line missing:\n%s", got)
	}
	if strings.Index(got, "notify =") > strings.Index(got, "[otel]") {
		t.Fatalf("notify must precede the first table:\n%s", got)
	}
	if !strings.Contains(got, "[otel]\nlog_user_prompt = false\n") {
		t.Fatalf("table content disturbed:\n%s", got)
	}
	st, _ = c.CodexNotify()
	if !st.Terma {
		t.Fatalf("status should report terma's notify: %+v", st)
	}
	if changed, err := c.InstallCodexNotify(); err != nil || changed {
		t.Fatalf("second install should be a no-op: changed=%v err=%v", changed, err)
	}
	changed, err = c.RemoveCodexNotify()
	if err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "notify") || !strings.Contains(string(data), "[otel]\nlog_user_prompt = false") {
		t.Fatalf("remove wrong:\n%s", data)
	}
}

func TestCodexNotifyChainsMultilineUserProgram(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	path := filepath.Join(home, "config.toml")
	original := "notify = [\n  \"my-notifier\",\n  \"--flag\",\n]\n\n[otel]\nlog_user_prompt = false\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Codex{}
	st, err := c.CodexNotify()
	if err != nil || !st.Configured || st.Terma {
		t.Fatalf("multiline notify should read as a foreign program: %+v (%v)", st, err)
	}
	if changed, err := c.InstallCodexNotify(); err != nil || !changed {
		t.Fatalf("multiline install: changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	if strings.Contains(got, "my-notifier") || strings.Contains(got, "--flag") {
		t.Fatalf("multiline notify not fully removed:\n%s", got)
	}
	if strings.Count(got, "notify =") != 1 || !strings.Contains(got, `notify = ["terma", "hook", "codex-notify"]`) {
		t.Fatalf("terma notify not installed cleanly:\n%s", got)
	}
	if !strings.Contains(got, "[otel]\nlog_user_prompt = false\n") {
		t.Fatalf("table content disturbed:\n%s", got)
	}
	if changed, err := c.RemoveCodexNotify(); err != nil || !changed {
		t.Fatalf("remove after multiline chain: changed=%v err=%v", changed, err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), `notify = ["my-notifier", "--flag"]`) {
		t.Fatalf("multiline notifier was not restored:\n%s", data)
	}
}

func TestCodexNotifyChainsAndRestoresUserProgram(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte("notify = [\"my-notifier\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Codex{}
	if changed, err := c.InstallCodexNotify(); err != nil || !changed {
		t.Fatalf("chain install: changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "my-notifier") || strings.Count(string(data), "notify =") != 1 {
		t.Fatalf("force should replace the single notify line:\n%s", data)
	}
	if changed, err := c.RemoveCodexNotify(); err != nil || !changed {
		t.Fatal("remove after force")
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), `notify = ["my-notifier"]`) {
		t.Fatalf("previous notifier was not restored:\n%s", data)
	}
	// A missing config file is "not configured", and removal is a no-op.
	_ = os.Remove(path)
	if st, err := c.CodexNotify(); err != nil || st.Configured {
		t.Fatalf("missing file: %+v %v", st, err)
	}
	if changed, err := c.RemoveCodexNotify(); err != nil || changed {
		t.Fatalf("remove on missing file: changed=%v err=%v", changed, err)
	}
}

// One record used to serve every Codex config on the machine. Connecting under a second
// CODEX_HOME overwrote the first config's displaced notifier, or — installing over no
// notifier there — deleted it, and the first disconnect had nothing to restore.
func TestCodexNotifyKeepsOneChainPerConfig(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	homeA, homeB, homeC := t.TempDir(), t.TempDir(), t.TempDir()
	write := func(home, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(homeA, "notify = [\"notifier-A\"]\n")
	write(homeB, "notify = [\"notifier-B\"]\n")
	write(homeC, "model = \"gpt-5.4\"\n") // no notifier: this install used to delete the record

	for _, home := range []string{homeA, homeB, homeC} {
		t.Setenv("CODEX_HOME", home)
		if _, err := (Codex{}).InstallCodexNotify(); err != nil {
			t.Fatalf("install under %s: %v", home, err)
		}
	}
	for home, want := range map[string]string{homeA: "notifier-A", homeB: "notifier-B"} {
		t.Setenv("CODEX_HOME", home)
		if _, err := (Codex{}).RemoveCodexNotify(); err != nil {
			t.Fatalf("remove under %s: %v", home, err)
		}
		data, _ := os.ReadFile(filepath.Join(home, "config.toml"))
		if !strings.Contains(string(data), want) || strings.Contains(string(data), "codex-notify") {
			t.Errorf("%s was not given its own notifier back:\n%s", want, data)
		}
	}
}

// The record the first chaining build wrote named no config. It still restores.
func TestCodexNotifyReadsTheLegacyRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	statePath, err := codexNotifyStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(`{"previous":["legacy-notifier","--flag"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(home, "config.toml")
	if err := os.WriteFile(cfg, []byte("notify = [\"terma\", \"hook\", \"codex-notify\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Codex{}).RemoveCodexNotify(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(cfg)
	if !strings.Contains(string(data), "legacy-notifier") {
		t.Fatalf("the legacy record's notifier was not restored:\n%s", data)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("an emptied record should be removed: %v", err)
	}
}
