package builtin

import (
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// Every catalog entry has a token and display name, its level folds its capabilities, and
// any less-than-full capability carries a note.
func TestSupportCatalogInvariants(t *testing.T) {
	cat := reg.SupportCatalog()
	if len(cat) == 0 {
		t.Fatal("support catalog is empty")
	}
	for _, a := range cat {
		if a.Name == "" || a.DisplayName == "" {
			t.Errorf("catalog entry missing name or display name: %+v", a)
		}
		want := agents.Overall(a.Attribution, a.Telemetry)
		if a.Support != want {
			t.Errorf("%s: overall support %q, want %q", a.Name, a.Support, want)
		}
		for label, c := range map[string]agents.CapabilitySupport{"attribution": a.Attribution, "telemetry": a.Telemetry} {
			if c.Level != agents.SupportFull && c.Note == "" {
				t.Errorf("%s %s is %q but has no note explaining the gap", a.Name, label, c.Level)
			}
		}
	}
}

// Cursor has full attribution but partial telemetry, so it reads as partial.
func TestSupportCatalogCursorIsPartial(t *testing.T) {
	cursor, ok := reg.LookupSupport("cursor")
	if !ok {
		t.Fatal("cursor missing from support catalog")
	}
	if cursor.Attribution.Level != agents.SupportFull {
		t.Errorf("cursor attribution = %q, want full", cursor.Attribution.Level)
	}
	if cursor.Telemetry.Level != agents.SupportPartial {
		t.Errorf("cursor telemetry = %q, want partial", cursor.Telemetry.Level)
	}
	if cursor.Support != agents.SupportPartial {
		t.Errorf("cursor overall = %q, want partial", cursor.Support)
	}
}

func TestOverallLevel(t *testing.T) {
	full := agents.CapabilitySupport{Level: agents.SupportFull}
	none := agents.CapabilitySupport{Level: agents.SupportNone}
	cases := []struct {
		name string
		caps []agents.CapabilitySupport
		want agents.SupportLevel
	}{
		{"all full", []agents.CapabilitySupport{full, full}, agents.SupportFull},
		{"mixed", []agents.CapabilitySupport{full, none}, agents.SupportPartial},
		{"all none", []agents.CapabilitySupport{none, none}, agents.SupportNone},
	}
	for _, tc := range cases {
		if got := agents.Overall(tc.caps...); got != tc.want {
			t.Errorf("%s: overall = %q, want %q", tc.name, got, tc.want)
		}
	}
}
