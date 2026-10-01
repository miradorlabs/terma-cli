package hookrun

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Evidence is read only from a regular, bounded, well-formed file, and each field is
// copied only in the shape it is allowed.
func TestEvidence(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for path, want := range map[string]string{
		filepath.Join(dir, "missing.json"): "missing",
		dir:                                "unsupported",
		write("big.json", `{"a":"`+strings.Repeat("x", EvidenceFileLimit)+`"}`): "oversized",
		write("bad.json", `{not json`):                                          "malformed",
		write("good.json", `{"a":1}`):                                           "present",
	} {
		if _, got := ReadEvidenceJSON(path); got != want {
			t.Errorf("%s: status %q, want %q", filepath.Base(path), got, want)
		}
	}
	var doc map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"plan":"team_tier_1","bad":"has space","on":true,"n":3.5,"huge":1e12,"neg":-1}`), &doc)
	dst := map[string]any{}
	CopyEvidenceString(dst, doc, "plan", "plan")
	CopyEvidenceString(dst, doc, "bad", "bad")
	CopyEvidenceBool(dst, doc, "on", "on")
	CopyEvidenceNumber(dst, doc, "n", "n", 10, false)
	CopyEvidenceNumber(dst, doc, "n", "whole", 10, true)
	CopyEvidenceNumber(dst, doc, "huge", "huge", 10, false)
	CopyEvidenceNumber(dst, doc, "neg", "neg", 10, false)
	if want := map[string]any{"plan": "team_tier_1", "on": true, "n": 3.5}; !maps.Equal(dst, want) {
		t.Fatalf("copied %v, want %v", dst, want)
	}
	for s, want := range map[string]bool{"a@b.co": true, "a@b": false, "@b.co": false, "a b@c.de": false, `a"@b.co`: false} {
		if ValidEmail(s) != want {
			t.Errorf("ValidEmail(%q) = %v", s, !want)
		}
	}
	for s, want := range map[string]bool{"12.345678901234567890": true, "0": true, "": false, "1.2.3": false, "-1": false, "1e3": false} {
		if ValidCreditBalance(s) != want {
			t.Errorf("ValidCreditBalance(%q) = %v", s, !want)
		}
	}
}
