package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The settings file is shared with hookmgr, which writes hook guards unescaped on
// purpose. A save here used to re-encode them as > and & — inside values this
// package never touched — so `terma install --telemetry` and a global connect churned
// a file customers commit.
func TestSaveLeavesShellOperatorsReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	const guard = `command -v terma >/dev/null 2>&1 && terma hook stop || true`
	original := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"` + guard + `"}]}]}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	s.merge(map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "a=b&c=<d>"})
	if err := s.save(true); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), `\u00`) {
		t.Fatalf("save HTML-escaped the document:\n%s", got)
	}
	for _, want := range []string{guard, "a=b&c=<d>"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("%q did not survive the save:\n%s", want, got)
		}
	}
}

// The status line entry is pre-encoded before it joins the document, so the final
// encoder alone would not have kept its redirect readable.
func TestStatusLineCommandIsWrittenUnescaped(t *testing.T) {
	encoded, err := marshalJSON(StatusLineCommand("my-line --flag"), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `\u00`) {
		t.Fatalf("status line command was HTML-escaped: %s", encoded)
	}
}
