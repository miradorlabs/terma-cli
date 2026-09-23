package harness

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

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
	changed, err := c.InstallStatusLine()
	if err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}
	sl := statusLineOf(t, path)
	cmd, _ := sl["command"].(string)
	if !strings.Contains(cmd, "terma hook statusline") || !strings.Contains(cmd, "exec /bin/sh -c 'npx -y ccstatusline@latest'") {
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
	// Idempotent.
	if changed, err := c.InstallStatusLine(); err != nil || changed {
		t.Fatalf("second install: changed=%v err=%v", changed, err)
	}
	if r, err := StatusLineRenderer(); err != nil || r != "npx -y ccstatusline@latest" {
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
	if r, _ := StatusLineRenderer(); r != "" {
		t.Fatalf("record must be gone, renderer %q", r)
	}
}

func TestInstallStatusLineWithoutOneAddsAndRemovesCleanly(t *testing.T) {
	c, path := claudeIn(t, `{"model": "opus"}`)
	if changed, err := c.InstallStatusLine(); err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	sl := statusLineOf(t, path)
	if cmd, _ := sl["command"].(string); !strings.HasSuffix(cmd, "|| exit 0") {
		t.Fatalf("no-renderer fallback: %q", cmd)
	}
	if r, _ := StatusLineRenderer(); r != "" {
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
	if _, err := c.InstallStatusLine(); err != nil {
		t.Fatal(err)
	}
	// The user swaps in a new tool by hand.
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
	// Re-installing wraps the new one.
	if _, err := c.InstallStatusLine(); err != nil {
		t.Fatal(err)
	}
	if r, _ := StatusLineRenderer(); r != "bun x ccstatusline" {
		t.Fatalf("renderer %q", r)
	}
}

func TestInstallStatusLineRefusesUnknownTypeAndRepositoryScope(t *testing.T) {
	c, _ := claudeIn(t, `{"statusLine": {"type": "widget", "command": "x"}}`)
	if _, err := c.InstallStatusLine(); err == nil {
		t.Fatal("unknown type must be refused")
	}
	if _, err := c.Local(t.TempDir()).(Claude).InstallStatusLine(); err == nil {
		t.Fatal("repository scope must be refused")
	}
}

func TestStatusLineCommandFallbackRunsThePreviousRendererWithoutTerma(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	// A previous command with quotes and a newline, run through the installed
	// string on a PATH without terma: the fallback branch must reproduce it.
	previous := "printf '%s|%s' \"it's\" 'two\nlines'"
	// A PATH with the system tools but no terma on it.
	cmd := exec.Command("sh", "-c", StatusLineCommand(previous))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil || string(out) != "it's|two\nlines" {
		t.Fatalf("fallback output %q err %v", out, err)
	}
	// Without a previous renderer the fallback is silent and successful.
	cmd = exec.Command("sh", "-c", StatusLineCommand(""))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	if out, err := cmd.Output(); err != nil || len(out) != 0 {
		t.Fatalf("silent fallback: %q %v", out, err)
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
			changed, err := c.InstallStatusLine()
			if err != nil || !changed {
				t.Fatalf("install %v %v", changed, err)
			}
			if renderer, err := StatusLineRenderer(); err != nil || renderer != command {
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
			if changed, err := c.InstallStatusLine(); err != nil || changed {
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
