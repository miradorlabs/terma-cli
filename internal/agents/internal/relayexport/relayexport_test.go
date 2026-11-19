package relayexport

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Refresh rewrites only a file whose configuration line render takes, keeping its mode.
func TestRefresh(t *testing.T) {
	t.Parallel()
	render := func(raw string) (string, bool) {
		if raw == "null" {
			return "", false
		}
		return "// this build\nconst CONFIG = " + raw + " /* terma:config */\n", true
	}
	for name, tc := range map[string]struct {
		file, want string
		changed    bool
	}{
		"missing":    {},
		"no line":    {file: "// a file of the developer's\n", want: "// a file of the developer's\n"},
		"inert":      {file: "const CONFIG = null /* terma:config */\n", want: "const CONFIG = null /* terma:config */\n"},
		"earlier":    {file: "// an earlier build\nconst CONFIG = {\"a\":1} /* terma:config */\n", want: "// this build\nconst CONFIG = {\"a\":1} /* terma:config */\n", changed: true},
		"this one's": {file: "// this build\nconst CONFIG = {\"a\":1} /* terma:config */\n", want: "// this build\nconst CONFIG = {\"a\":1} /* terma:config */\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "plugin.mjs")
			if tc.file != "" {
				if err := os.WriteFile(path, []byte(tc.file), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			changed, err := Refresh(path, "const CONFIG = ", " /* terma:config */", render)
			if err != nil || changed != tc.changed {
				t.Fatalf("changed=%v err=%v, want changed=%v", changed, err, tc.changed)
			}
			got, err := os.ReadFile(path)
			if tc.file == "" {
				if !os.IsNotExist(err) {
					t.Fatalf("a missing file was written: %v", err)
				}
				return
			}
			if string(got) != tc.want {
				t.Errorf("file:\n%s\nwant:\n%s", got, tc.want)
			}
			if info, _ := os.Stat(path); runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
				t.Errorf("mode %v, want the file's own 0640", info.Mode().Perm())
			}
		})
	}
}
