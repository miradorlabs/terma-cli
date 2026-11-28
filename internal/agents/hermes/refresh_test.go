package hermes

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHermesRefreshMachineUpdatesInstalledPlugin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERMES_HOME", dir)
	path := filepath.Join(dir, "plugins", "terma", "__init__.py")

	if got, changed, err := (Agent{}).RefreshMachine(); err != nil || changed || got != path {
		t.Fatalf("RefreshMachine(absent) = (%q, %v, %v); want (%q, false, nil)", got, changed, err, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `"""Terma old hermes plugin"""
CONFIG_JSON = "{\"version\":1,\"endpoint\":\"http://127.0.0.1:43180\",\"headers\":{\"Authorization\":\"Bearer tok\"},\"includePrompts\":true,\"includeToolContent\":true,\"hookCommand\":[\"/custom/terma\",\"hook\"]}"  # terma:config
def register(ctx): pass
`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := (Agent{}).RefreshMachine(); err != nil || !changed {
		t.Fatalf("RefreshMachine(outdated) = (%v, %v); want (true, nil)", changed, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`\"/custom/terma\"`, `http://127.0.0.1:43180`, `"hermes-file-edit"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("refreshed plugin missing %s", want)
		}
	}
	if _, changed, err := (Agent{}).RefreshMachine(); err != nil || changed {
		t.Fatalf("RefreshMachine(current) = (%v, %v); want (false, nil)", changed, err)
	}
}

func TestHermesHookWorkerWaitsForConfirmedExitUnderPython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook test")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	text, err := renderPlugin(pluginConfig{Endpoint: "http://127.0.0.1:43180", Headers: map[string]string{}, HookCommand: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "terma_plugin.py"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := `
import json, subprocess, sys, threading, time
sys.path.insert(0, sys.argv[1])
import terma_plugin as p

started = []
release = threading.Event()
second_started = threading.Event()

class StubProc:
    def __init__(self, event):
        self.event = event
        self.returncode = None
        started.append(event)
        if event == "second":
            second_started.set()
    def communicate(self, data, timeout=None):
        if self.event == "first":
            raise subprocess.TimeoutExpired(cmd="first", timeout=timeout)
        self.returncode = 0
        return (b"", b"")
    def poll(self):
        return self.returncode
    def kill(self):
        if not release.is_set():
            raise OSError("transient kill error")
        self.returncode = -9
    def wait(self, timeout=None):
        if not release.wait(timeout=min(timeout or 0.1, 0.05)):
            raise subprocess.TimeoutExpired(cmd=self.event, timeout=timeout)
        self.returncode = -9
        return -9

p.subprocess.Popen = lambda cmd, **kw: StubProc(cmd[-1])
ok1 = p._terma_hook("first", {}, wait=True)
p._terma_hook("second", {}, wait=False)
time.sleep(0.2)
started_while_alive = second_started.is_set()
release.set()
started_after_exit = second_started.wait(timeout=2.0)
print(json.dumps({"ok1": ok1, "started_while_alive": started_while_alive, "started_after_exit": started_after_exit, "started": started}))
`
	out, err := exec.Command(python, "-c", driver, dir).CombinedOutput()
	if err != nil {
		t.Fatalf("python: %v\n%s", err, out)
	}
	var got struct {
		OK1               bool     `json:"ok1"`
		StartedWhileAlive bool     `json:"started_while_alive"`
		StartedAfterExit  bool     `json:"started_after_exit"`
		Started           []string `json:"started"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse output %q: %v", out, err)
	}
	if got.OK1 || got.StartedWhileAlive || !got.StartedAfterExit {
		t.Fatalf("Hermes FIFO confirmed-exit invariant violated: %+v", got)
	}
}
