package reach

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

func TestSpecsExtractPathsGlobsListsAndShellCommands(t *testing.T) {
	ws := t.TempDir()
	repoA, repoB := filepath.Join(ws, "repoA"), filepath.Join(ws, "repoB")
	for _, f := range []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go", "g.go", "h.go", "i.go"} {
		p := filepath.Join(repoB, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(repoA, 0o755); err != nil {
		t.Fatal(err)
	}
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	cases := []struct {
		name  string
		spec  Spec
		input string
		cwd   string
		want  string
	}{
		{"copilot read", Copilot, `{"filePath":` + q(filepath.Join(repoB, "a.go")) + `}`, repoA, filepath.Join(repoB, "a.go")},
		{"copilot shell cwd", Copilot, `{"command":"cat b.go","cwd":` + q(repoB) + `}`, repoA, repoB},
		{"cursor read", Cursor, `{"target_file":` + q(filepath.Join(repoB, "c.go")) + `}`, repoA, filepath.Join(repoB, "c.go")},
		{"cursor shell", Cursor, `{"command":"cat ../repoB/d.go"}`, repoA, repoB},
		{"gemini read_many", Gemini, `{"paths":[` + q(filepath.Join(repoB, "e.go")) + `,` + q(filepath.Join(repoB, "f.go")) + `]}`, repoA, filepath.Join(repoB, "e.go")},
		{"gemini shell dir", Gemini, `{"command":"cat g.go","directory":"../repoB"}`, repoA, repoB},
		{"antigravity Path", Antigravity, `{"Path":` + q(filepath.Join(repoB, "h.go")) + `}`, repoA, filepath.Join(repoB, "h.go")},
		{"antigravity run_command", Antigravity, `{"CommandLine":"cat i.go","Cwd":` + q(repoB) + `}`, repoA, repoB},
		{"antigravity list_dir", Antigravity, `{"DirectoryPath":` + q(repoB) + `}`, repoA, repoB},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.spec.Of(json.RawMessage(tc.input), tc.cwd)
			if !slices.ContainsFunc(got, func(p string) bool { return strings.Contains(p, tc.want) }) {
				t.Fatalf("Of(%s) = %v; want entry containing %q", tc.input, got, tc.want)
			}
		})
	}
	got := Cursor.Of(json.RawMessage(`{"command":"eval $CMD"}`), repoA)
	if !slices.ContainsFunc(got, func(p string) bool { return strings.Contains(p, hookrun.UnknownReach) }) {
		t.Fatalf("eval command = %v; want UnknownReach marker", got)
	}
}
