package hookmgr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// HookedTerma reads back the termas a hooks file's entries run, however the agent nests
// them: each path once, unquoted, and nothing from a command terma did not write or from a
// managed entry, which names terma through $HOME.
func TestHookedTerma(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mine, older, quoted := "/opt/homebrew/bin/terma", "/Users/dev/.local/bin/terma", "/Users/it's/bin/terma"
	doc := map[string]any{
		"model": "opus",
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"hooks": []any{
				map[string]any{"type": "command", "command": UserHookCommand(mine)("session-start")},
				map[string]any{"type": "command", "command": "~/bin/notify.sh"},
			}}},
			"Stop": []any{map[string]any{"hooks": []any{
				map[string]any{"type": "command", "command": UserHookCommand(older)("stop")},
				map[string]any{"type": "command", "command": UserHookCommand(mine)("stop"), "powershell": "C:\\terma.exe hook --user stop"},
			}}},
			"PreToolUse": []any{map[string]any{"command": UserHookCommand(quoted)("pre-tool-use")}},
			"Managed":    []any{map[string]any{"command": ManagedHookCommand("$HOME/.local/bin/terma")("stop")}},
		},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := HookedTerma(path)
	if want := []string{older, quoted, mine}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("HookedTerma = %q, %v; want %q", got, err, want)
	}

	if got, err := HookedTerma(filepath.Join(dir, "missing.json")); err != nil || got != nil {
		t.Fatalf("a missing file lists %q, %v", got, err)
	}
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte(`{"hooks": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := HookedTerma(broken); err == nil {
		t.Fatal("a hooks file that cannot be read listed no terma")
	}
}
