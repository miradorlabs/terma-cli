package routing

import (
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
