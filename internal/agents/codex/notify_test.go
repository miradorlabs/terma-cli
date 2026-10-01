package codex

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
	c := exporter{}
	if err := os.WriteFile(path, []byte("notify = [\"notifier-A\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.installNotify(); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := os.WriteFile(path, []byte("model = \"gpt-5.4\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.installNotify(); err != nil {
		t.Fatalf("reinstall over no notifier: %v", err)
	}
	if _, err := c.removeNotify(); err != nil {
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
	cp, err := (exporter{}).ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	// A user program whose path merely contains "terma" and "codex-notify" is not refused as
	// a recursive chain.
	if err := saveCodexNotifyChain(cp, []string{"/opt/terma-tools/codex-notify-desktop"}); err != nil {
		t.Fatal(err)
	}
	if err := runPreviousNotify(context.Background(), "{}"); err != nil && strings.Contains(err.Error(), "recursive") {
		t.Fatalf("legitimate notifier refused as recursive: %v", err)
	}
	if err := saveCodexNotifyChain(cp, notifyCommand); err != nil {
		t.Fatal(err)
	}
	if err := runPreviousNotify(context.Background(), "{}"); err == nil || !strings.Contains(err.Error(), "recursive") {
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
	c := exporter{}
	st, err := c.notifySetting()
	if err != nil || st.Configured {
		t.Fatalf("unexpected status %+v (%v)", st, err)
	}
	changed, err := c.installNotify()
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
	st, _ = c.notifySetting()
	if !st.Terma {
		t.Fatalf("status should report terma's notify: %+v", st)
	}
	if changed, err := c.installNotify(); err != nil || changed {
		t.Fatalf("second install should be a no-op: changed=%v err=%v", changed, err)
	}
	changed, err = c.removeNotify()
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
	c := exporter{}
	st, err := c.notifySetting()
	if err != nil || !st.Configured || st.Terma {
		t.Fatalf("multiline notify should read as a foreign program: %+v (%v)", st, err)
	}
	if changed, err := c.installNotify(); err != nil || !changed {
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
	if changed, err := c.removeNotify(); err != nil || !changed {
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
	c := exporter{}
	if changed, err := c.installNotify(); err != nil || !changed {
		t.Fatalf("chain install: changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "my-notifier") || strings.Count(string(data), "notify =") != 1 {
		t.Fatalf("force should replace the single notify line:\n%s", data)
	}
	if changed, err := c.removeNotify(); err != nil || !changed {
		t.Fatal("remove after force")
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), `notify = ["my-notifier"]`) {
		t.Fatalf("previous notifier was not restored:\n%s", data)
	}
	// A missing config file is "not configured", and removal is a no-op.
	_ = os.Remove(path)
	if st, err := c.notifySetting(); err != nil || st.Configured {
		t.Fatalf("missing file: %+v %v", st, err)
	}
	if changed, err := c.removeNotify(); err != nil || changed {
		t.Fatalf("remove on missing file: changed=%v err=%v", changed, err)
	}
}

// Each Codex config keeps its own displaced notifier, including a config that had none.
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
	write(homeC, "model = \"gpt-5.4\"\n")

	for _, home := range []string{homeA, homeB, homeC} {
		t.Setenv("CODEX_HOME", home)
		if _, err := (exporter{}).installNotify(); err != nil {
			t.Fatalf("install under %s: %v", home, err)
		}
	}
	for home, want := range map[string]string{homeA: "notifier-A", homeB: "notifier-B"} {
		t.Setenv("CODEX_HOME", home)
		if _, err := (exporter{}).removeNotify(); err != nil {
			t.Fatalf("remove under %s: %v", home, err)
		}
		data, _ := os.ReadFile(filepath.Join(home, "config.toml"))
		if !strings.Contains(string(data), want) || strings.Contains(string(data), "codex-notify") {
			t.Errorf("%s was not given its own notifier back:\n%s", want, data)
		}
	}
}
