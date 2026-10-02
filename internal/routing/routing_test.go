package routing

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	testProjectID = "770e8400-e29b-41d4-a716-446655440000"
	testEndpoint  = "https://otel-dev.example.com"
)

// sandbox points config.Dir() at a temporary directory.
func sandbox(t *testing.T) {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

func TestRecordRoundTrip(t *testing.T) {
	sandbox(t)
	in := Record{ProjectID: testProjectID, Endpoint: testEndpoint, Signals: []string{"traces", "logs"}, Harnesses: []string{"claude", "codex"}}
	if err := SaveRecord(in); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadRecord(testProjectID)
	if err != nil || !ok {
		t.Fatalf("LoadRecord: ok=%v err=%v", ok, err)
	}
	if got.Endpoint != testEndpoint || len(got.Harnesses) != 2 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestRecordRejectsUnsafeProjectID(t *testing.T) {
	sandbox(t)
	if err := SaveRecord(Record{ProjectID: "../escape"}); err == nil {
		t.Fatal("expected an unsafe project id to be rejected")
	}
}

// An earlier terma's content switches are read as nothing and gone from the next write:
// content is the team's policy alone.
func TestARecordsOldContentSwitchesAreDroppedOnRewrite(t *testing.T) {
	sandbox(t)
	d, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"project_id":"` + testProjectID + `","endpoint":"` + testEndpoint + `","signals":["logs"],"include_prompts":false,"include_tool_content":false,"harnesses":["codex"]}`
	path := filepath.Join(d, testProjectID+".json")
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := LoadRecord(testProjectID)
	if err != nil || !ok || !slices.Equal(rec.Signals, []string{"logs"}) || !slices.Equal(rec.Harnesses, []string{"codex"}) {
		t.Fatalf("an old record did not load: %+v, %v, %v", rec, ok, err)
	}
	if err := SaveRecord(rec); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "include_") {
		t.Fatalf("the rewrite kept a content switch:\n%s", data)
	}
}
