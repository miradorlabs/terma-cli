package doctor

import (
	"bytes"
	"strings"
	"testing"
)

func TestReadinessSummary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		checks []Check
		want   string
		failed bool
	}{
		{"ready", []Check{{Status: Pass}}, "Setup ready", false},
		{"partial export", []Check{{Status: Warn, Ready: 1, Of: 2, Fix: "open a new terminal"}}, "open a new terminal", false},
		{"unverified backend", []Check{{Key: KeyBackend, Status: Warn, Inconclusive: true, Fix: "terma doctor"}}, "terma doctor", false},
		{"broken backend", []Check{{Key: KeyBackend, Status: Fail, Fix: "check network"}}, "check network", true},
		{"skipped probe", []Check{{Key: KeyScratch, Status: Skip}}, "Verification incomplete", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Build(tc.checks)
			var out bytes.Buffer
			RenderSummary(&out, r)
			if !strings.Contains(out.String(), tc.want) || strings.Contains(out.String(), "%") || r.Failed() != tc.failed {
				t.Fatalf("summary=%q failed=%v", out.String(), r.Failed())
			}
		})
	}
}

func TestReadinessDeduplicatesActions(t *testing.T) {
	var out bytes.Buffer
	RenderSummary(&out, Build([]Check{
		{Status: Warn, Fix: "open a new terminal"},
		{Status: Warn, Fix: "open a new terminal"},
		{Status: Fail, Fix: "terma install"},
	}))
	if strings.Count(out.String(), "open a new terminal") != 1 || !strings.Contains(out.String(), "terma install") {
		t.Fatal(out.String())
	}
}

func TestRenderCheck(t *testing.T) {
	var out bytes.Buffer
	RenderCheck(&out, Check{Status: Fail, Name: "commit hooks installed", Detail: "hooks missing", Fix: "terma install"}, NameWidth)
	if !strings.Contains(out.String(), "FAIL  commit hooks installed") || !strings.Contains(out.String(), "→ terma install") {
		t.Fatal(out.String())
	}
}
