package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The service definitions: valid (plutil lints the plist on macOS), running the relay
// with no idle exit, carrying the config directory, and named per config directory so
// a sandbox never touches the real service.
func TestDefinitions(t *testing.T) {
	env := map[string]string{"TERMA_CONFIG_DIR": "/tmp/a & b", "TERMA_ENV": "dev"}
	plist := Launchd("ai.terma.relay.x", "/opt/terma/bin/terma", "/tmp/log", env)
	for _, want := range []string{"<string>relay</string><string>run</string><string>--idle</string><string>0</string>", "<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>", "/tmp/a &amp; b"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	if _, err := exec.LookPath("plutil"); err == nil {
		path := filepath.Join(t.TempDir(), "p.plist")
		_ = os.WriteFile(path, []byte(plist), 0o644)
		if out, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
			t.Fatalf("plutil: %v\n%s", err, out)
		}
	}
	unit := Systemd("/opt/terma/bin/terma", env)
	for _, want := range []string{`ExecStart="/opt/terma/bin/terma" relay run --idle 0 --quiet`, `Environment="TERMA_CONFIG_DIR=/tmp/a & b"`, "Restart=on-failure"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	vbs := Windows(`C:\Users\a "b"\terma.exe`, env)
	for _, want := range []string{`env("TERMA_CONFIG_DIR") = "/tmp/a & b"`, `shell.Run """C:\Users\a ""b""\terma.exe"" relay supervise", 0, False`} {
		if !strings.Contains(vbs, want) {
			t.Errorf("launcher lacks %q:\n%s", want, vbs)
		}
	}
}
