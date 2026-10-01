package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// canPrompt is prompt.Interactive's only caller: Interactive checks stdin alone, so an
// agent with a pty would get the raw-mode picker.
func TestPromptsAreGatedOnCanPrompt(t *testing.T) {
	direct := regexp.MustCompile(`\bprompt\.Interactive\(\)`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if direct.MatchString(line) && !strings.Contains(line, "return output.Interactive() && prompt.Interactive()") {
				t.Errorf("%s:%d asks prompt.Interactive() directly; use canPrompt(): %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}
