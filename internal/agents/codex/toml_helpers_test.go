package codex

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, raw string) map[string]any {
	t.Helper()
	doc := map[string]any{}
	if strings.TrimSpace(raw) == "" {
		return doc
	}
	if err := unmarshalTOMLLenient([]byte(raw), &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return doc
}
