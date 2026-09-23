package harness

import "testing"

// The catalog is what `terma harness list` reports, so its shape is a contract: every
// agent has a token and a display name, its overall level is the fold of its
// capabilities, and any less-than-full capability carries a note explaining the gap.
func TestSupportCatalogInvariants(t *testing.T) {
	cat := SupportCatalog()
	if len(cat) == 0 {
		t.Fatal("support catalog is empty")
	}
	for _, a := range cat {
		if a.Name == "" || a.DisplayName == "" {
			t.Errorf("catalog entry missing name or display name: %+v", a)
		}
		want := overall(a.Attribution, a.Telemetry)
		if a.Support != want {
			t.Errorf("%s: overall support %q, want %q", a.Name, a.Support, want)
		}
		for label, c := range map[string]CapabilitySupport{"attribution": a.Attribution, "telemetry": a.Telemetry} {
			if c.Level != SupportFull && c.Note == "" {
				t.Errorf("%s %s is %q but has no note explaining the gap", a.Name, label, c.Level)
			}
		}
	}
}

// The partial case is the whole reason the command exists: Cursor does attribution but
// exports partial hook evidence, so it must read as partial, not full and not none.
func TestSupportCatalogCursorIsPartial(t *testing.T) {
	cursor, ok := LookupSupport("cursor")
	if !ok {
		t.Fatal("cursor missing from support catalog")
	}
	if cursor.Attribution.Level != SupportFull {
		t.Errorf("cursor attribution = %q, want full", cursor.Attribution.Level)
	}
	if cursor.Telemetry.Level != SupportPartial {
		t.Errorf("cursor telemetry = %q, want partial", cursor.Telemetry.Level)
	}
	if cursor.Support != SupportPartial {
		t.Errorf("cursor overall = %q, want partial", cursor.Support)
	}
}

// Every telemetry harness in the registry is a full agent in the catalog, so the two
// views never disagree about an agent terma actively exports for.
func TestSupportCatalogCoversTelemetryRegistry(t *testing.T) {
	for _, h := range All() {
		a, ok := LookupSupport(h.Name())
		if !ok {
			t.Errorf("telemetry harness %q is not in the support catalog", h.Name())
			continue
		}
		if a.Telemetry.Level != SupportFull {
			t.Errorf("%s exports telemetry but catalog says %q", h.Name(), a.Telemetry.Level)
		}
		if a.Support != SupportFull {
			t.Errorf("%s is a telemetry harness but catalog overall is %q", h.Name(), a.Support)
		}
	}
}

func TestOverallLevel(t *testing.T) {
	full := CapabilitySupport{Level: SupportFull}
	none := CapabilitySupport{Level: SupportNone}
	cases := []struct {
		name string
		caps []CapabilitySupport
		want SupportLevel
	}{
		{"all full", []CapabilitySupport{full, full}, SupportFull},
		{"mixed", []CapabilitySupport{full, none}, SupportPartial},
		{"all none", []CapabilitySupport{none, none}, SupportNone},
	}
	for _, tc := range cases {
		if got := overall(tc.caps...); got != tc.want {
			t.Errorf("%s: overall = %q, want %q", tc.name, got, tc.want)
		}
	}
}
