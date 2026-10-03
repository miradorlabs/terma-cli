package project

import (
	"strings"
	"testing"
)

func TestValidID(t *testing.T) {
	if ValidID("") || ValidID(strings.Repeat("a", maxIDLen+1)) {
		t.Fatal("empty and over-long ids must be rejected")
	}
	if !ValidID("a") || !ValidID("770e8400-e29b-41d4-a716-446655440000") {
		t.Fatal("a real id must be accepted")
	}
	for _, unsafe := range []string{"../../.zshenv", ".hidden", "a/b", `a\b`} {
		if ValidID(unsafe) {
			t.Errorf("ValidID(%q) = true", unsafe)
		}
	}
}
