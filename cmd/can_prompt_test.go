package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A prompt is shown only when canPrompt says a person is there: a terminal on both ends
// and no agent driving it. prompt.Interactive answers the stdin half alone, and install
// and setup asked it directly — so an agent with a pty got the raw-mode picker and the
// question about its shell startup file. canPrompt is the only caller it should have.
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
