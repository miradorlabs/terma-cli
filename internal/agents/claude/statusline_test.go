package claude

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testTerma is where the tests' status line runs terma.
const testTerma = "/opt/terma/bin/terma"

func statusLineOf(t *testing.T, path string) map[string]any {
	t.Helper()
	doc := readJSON(t, path)
	sl, _ := doc["statusLine"].(map[string]any)
	return sl
}

func TestInstallStatusLineWrapsAndKeepsEveryOption(t *testing.T) {
	c, path := claudeIn(t, `{
  "model": "opus",
  "statusLine": {"type": "command", "command": "npx -y ccstatusline@latest", "padding": 2, "refreshInterval": 5, "hideVimModeIndicator": true},
  "hooks": {"Stop": []}
}`)
	changed, err := c.InstallStatusLine(testTerma)
	if err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}
	sl := statusLineOf(t, path)
	cmd, _ := sl["command"].(string)
	if !strings.Contains(cmd, "exec '"+testTerma+"' hook statusline") || !strings.Contains(cmd, "exec /bin/sh -c 'npx -y ccstatusline@latest'") {
		t.Fatalf("command %q", cmd)
	}
	for k, want := range map[string]any{"type": "command", "padding": 2.0, "refreshInterval": 5.0, "hideVimModeIndicator": true} {
		if sl[k] != want {
			t.Errorf("%s = %v, want %v", k, sl[k], want)
		}
	}
	doc := readJSON(t, path)
	if doc["model"] != "opus" || doc["hooks"] == nil {
		t.Fatal("unrelated settings must survive")
	}
	if changed, err := c.InstallStatusLine(testTerma); err != nil || changed {
		t.Fatalf("second install: changed=%v err=%v", changed, err)
	}
	if r, err := statusLineRenderer(c.dir); err != nil || r != "npx -y ccstatusline@latest" {
		t.Fatalf("renderer %q err %v", r, err)
	}
	st, err := c.StatusLineState("")
	if err != nil || !st.Installed || st.Renderer != "npx -y ccstatusline@latest" || st.Replaced {
		t.Fatalf("state %+v err %v", st, err)
	}
	// Remove restores the original object exactly.
	changed, err = c.RemoveStatusLine()
	if err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	sl = statusLineOf(t, path)
	if sl["command"] != "npx -y ccstatusline@latest" || sl["padding"] != 2.0 || sl["refreshInterval"] != 5.0 || sl["hideVimModeIndicator"] != true {
		t.Fatalf("restored %v", sl)
	}
	if r, _ := statusLineRenderer(c.dir); r != "" {
		t.Fatalf("record must be gone, renderer %q", r)
	}
}

func TestInstallStatusLineWithoutOneAddsAndRemovesCleanly(t *testing.T) {
	c, path := claudeIn(t, `{"model": "opus"}`)
	if changed, err := c.InstallStatusLine(testTerma); err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	sl := statusLineOf(t, path)
	if cmd, _ := sl["command"].(string); !strings.HasSuffix(cmd, "|| exit 0") {
		t.Fatalf("no-renderer fallback: %q", cmd)
	}
	if r, _ := statusLineRenderer(c.dir); r != "" {
		t.Fatalf("renderer %q", r)
	}
	if changed, err := c.RemoveStatusLine(); err != nil || !changed {
		t.Fatalf("remove: %v %v", changed, err)
	}
	if _, ok := readJSON(t, path)["statusLine"]; ok {
		t.Fatal("statusLine must be gone after remove")
	}
	if readJSON(t, path)["model"] != "opus" {
		t.Fatal("unrelated settings must survive")
	}
}

func TestRemoveStatusLineLeavesAUserReplacementAlone(t *testing.T) {
	c, path := claudeIn(t, `{"statusLine": {"type": "command", "command": "~/.claude/statusline.sh"}}`)
	if _, err := c.InstallStatusLine(testTerma); err != nil {
		t.Fatal(err)
	}
	doc := readJSON(t, path)
	doc["statusLine"] = map[string]any{"type": "command", "command": "bun x ccstatusline"}
	data, _ := json.Marshal(doc)
	_ = os.WriteFile(path, data, 0o600)

	st, err := c.StatusLineState("")
	if err != nil || st.Installed || !st.Replaced {
		t.Fatalf("state %+v err %v", st, err)
	}
	changed, err := c.RemoveStatusLine()
	if err != nil || changed {
		t.Fatalf("remove must not touch the user's entry: %v %v", changed, err)
	}
	if statusLineOf(t, path)["command"] != "bun x ccstatusline" {
		t.Fatal("user's entry changed")
	}
	if _, err := c.InstallStatusLine(testTerma); err != nil {
		t.Fatal(err)
	}
	if r, _ := statusLineRenderer(c.dir); r != "bun x ccstatusline" {
		t.Fatalf("renderer %q", r)
	}
}

func TestInstallStatusLineRefusesUnknownTypeAndRepositoryScope(t *testing.T) {
	c, _ := claudeIn(t, `{"statusLine": {"type": "widget", "command": "x"}}`)
	if _, err := c.InstallStatusLine(testTerma); err == nil {
		t.Fatal("unknown type must be refused")
	}
	local, _ := c.Local(t.TempDir())
	if _, err := local.(exporter).InstallStatusLine(testTerma); err == nil {
		t.Fatal("repository scope must be refused")
	}
}

func TestStatusLineCommandFallbackRunsThePreviousRendererWithoutTerma(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	// A previous command with quotes and a newline, run once terma has gone, is reproduced.
	gone := filepath.Join(t.TempDir(), "terma")
	previous := "printf '%s|%s' \"it's\" 'two\nlines'"
	cmd := exec.Command("sh", "-c", statusLineCommand(gone, previous))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil || string(out) != "it's|two\nlines" {
		t.Fatalf("fallback output %q err %v", out, err)
	}
	// Without a previous renderer the fallback is silent and successful.
	cmd = exec.Command("sh", "-c", statusLineCommand(gone, ""))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	if out, err := cmd.Output(); err != nil || len(out) != 0 {
		t.Fatalf("silent fallback: %q %v", out, err)
	}
}

// The wrap runs terma at the path it was installed with, never a terma the PATH finds, a
// relative entry in the session's directory included; with that one gone it falls back to
// the renderer, whatever else is on the PATH.
func TestStatusLineCommandRunsTermaByItsPathNeverThePATH(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	fake := func(path, says string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '"+says+" %s' \"$*\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	installed := filepath.Join(t.TempDir(), "it's terma", "terma")
	fake(installed, "installed")
	session := t.TempDir()
	fake(filepath.Join(session, "bin", "terma"), "shadow")
	onPath := t.TempDir()
	fake(filepath.Join(onPath, "terma"), "shadow")
	run := func() string {
		t.Helper()
		cmd := exec.Command("sh", "-c", statusLineCommand(installed, "printf renderer"))
		cmd.Dir = session
		cmd.Env = []string{"PATH=bin:" + onPath + ":/usr/bin:/bin"}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("status line: %v", err)
		}
		return string(out)
	}
	if got := run(); got != "installed hook statusline" {
		t.Fatalf("ran %q, want the installed terma", got)
	}
	if err := os.Remove(installed); err != nil {
		t.Fatal(err)
	}
	if got := run(); got != "renderer" {
		t.Fatalf("ran %q, want the renderer once terma has gone", got)
	}
}

// terma's own command is recognized in every form a build wrote, by its shape alone.
func TestStatusLineCommandIsRecognizedByItsShape(t *testing.T) {
	t.Parallel()
	for _, command := range []string{
		statusLineCommand(testTerma, ""),
		statusLineCommand("/Users/o'neil/.local/bin/terma", "npx ccstatusline"),
		legacyStatusLineGuard + "exit 0",
		"terma hook statusline",
		"exec terma hook statusline",
	} {
		if !isStatusLineCommand(command) {
			t.Errorf("not recognized: %q", command)
		}
	}
	for _, command := range []string{
		`[ -x '/opt/a/terma' ] && exec '/opt/b/terma' hook statusline || exit 0`,
		`printf '[ -x '/x' ] && exec '/x' hook statusline || '`,
		`printf 'terma hook statusline'`,
	} {
		if isStatusLineCommand(command) {
			t.Errorf("recognized as terma's: %q", command)
		}
	}
}

// An earlier build's wrap, which found terma on the PATH, is rewritten to run terma by its
// path, by a refresh and by setup alike, keeping the renderer and every option.
func TestALegacyWrapIsRewrittenToRunTermaByItsPath(t *testing.T) {
	for name, apply := range map[string]func(exporter) (bool, error){
		"refresh": func(c exporter) (bool, error) { _, changed, err := c.RefreshStatusLine(testTerma); return changed, err },
		"setup":   func(c exporter) (bool, error) { return c.InstallStatusLine(testTerma) },
	} {
		t.Run(name, func(t *testing.T) {
			c, path := claudeIn(t, `{"statusLine": {"type": "command", "command": "my-renderer", "padding": 1}}`)
			if _, err := c.InstallStatusLine(testTerma); err != nil {
				t.Fatal(err)
			}
			s, err := loadSettings(path)
			if err != nil {
				t.Fatal(err)
			}
			legacy := legacyStatusLineGuard + "exec /bin/sh -c 'my-renderer'"
			s.root[claudeStatusLineKey], _ = json.Marshal(map[string]any{"type": "command", "command": legacy, "padding": 1})
			if err := s.save(false); err != nil {
				t.Fatal(err)
			}
			if st, err := c.StatusLineState(""); err != nil || !st.Installed {
				t.Fatalf("the legacy wrap is not terma's: %+v %v", st, err)
			}
			if changed, err := apply(c); err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			sl := statusLineOf(t, path)
			if sl["command"] != statusLineCommand(testTerma, "my-renderer") || sl["padding"] != 1.0 {
				t.Fatalf("rewritten entry %v", sl)
			}
			if changed, err := apply(c); err != nil || changed {
				t.Fatalf("second pass: changed=%v err=%v", changed, err)
			}
			if _, err := c.RemoveStatusLine(); err != nil {
				t.Fatal(err)
			}
			if sl := statusLineOf(t, path); sl["command"] != "my-renderer" {
				t.Fatalf("restored %v", sl)
			}
		})
	}
}

// Only an absolute path to terma is ever written.
func TestStatusLineRefusesARelativeTerma(t *testing.T) {
	c, path := claudeIn(t, `{"statusLine": {"type": "command", "command": "my-renderer"}}`)
	for _, terma := range []string{"terma", "bin/terma", ""} {
		if _, err := c.InstallStatusLine(terma); err == nil {
			t.Errorf("installed with terma at %q", terma)
		}
		if _, _, err := c.RefreshStatusLine(terma); err == nil {
			t.Errorf("refreshed with terma at %q", terma)
		}
	}
	if sl := statusLineOf(t, path); sl["command"] != "my-renderer" {
		t.Fatalf("settings changed: %v", sl)
	}
}

func TestStatusLineOverridesAreReported(t *testing.T) {
	c, _ := claudeIn(t, `{}`)
	repo := t.TempDir()
	_ = os.MkdirAll(repo+"/.claude", 0o755)
	_ = os.WriteFile(repo+"/.claude/settings.json", []byte(`{"statusLine":{"type":"command","command":"echo team"}}`), 0o644)
	st, err := c.StatusLineState(repo)
	if err != nil || len(st.Overrides) != 1 || !strings.HasSuffix(st.Overrides[0], ".claude/settings.json") {
		t.Fatalf("overrides %+v err %v", st.Overrides, err)
	}
}

func TestStatusLineExoticCommandsAndOptionsRoundTrip(t *testing.T) {
	for name, command := range map[string]string{
		"npx-with-options": `FORCE_COLOR=3 npx --yes ccstatusline@2.2.29 --config '/tmp/my theme.json'`,
		"quoted-path":      `bash "$HOME/status line's theme.sh" --label "it's 🌈"`,
		"pipeline":         `cat | jq -r '.model.display_name' | sed 's/^/λ /'`,
		"multiline":        "cat <<'EOF'\n$literal `backticks` \\slashes\nEOF\n",
		"literal-marker":   `printf 'terma hook statusline'`,
	} {
		t.Run(name, func(t *testing.T) {
			original := map[string]any{"type": "command", "command": command, "padding": 3, "refreshInterval": 0.25,
				"hideVimModeIndicator": true, "futureOption": map[string]any{"nested": []any{"🎨", false, nil, 42}}}
			data, _ := json.Marshal(map[string]any{"statusLine": original})
			c, path := claudeIn(t, string(data))
			changed, err := c.InstallStatusLine(testTerma)
			if err != nil || !changed {
				t.Fatalf("install %v %v", changed, err)
			}
			if renderer, err := statusLineRenderer(c.dir); err != nil || renderer != command {
				t.Fatalf("renderer=%q err=%v", renderer, err)
			}
			wrapped := statusLineOf(t, path)
			for key, value := range original {
				if key == "command" {
					continue
				}
				want, _ := json.Marshal(value)
				got, _ := json.Marshal(wrapped[key])
				if string(got) != string(want) {
					t.Fatalf("option %s: %s != %s", key, got, want)
				}
			}
			if changed, err := c.InstallStatusLine(testTerma); err != nil || changed {
				t.Fatal("not idempotent", err)
			}
			if _, err := c.RemoveStatusLine(); err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(statusLineOf(t, path))
			want, _ := json.Marshal(original)
			if string(got) != string(want) {
				t.Fatalf("restore %s != %s", got, want)
			}
		})
	}
}
