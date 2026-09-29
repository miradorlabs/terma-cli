package cmd

import (
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/compat"
	"github.com/miradorlabs/terma-cli/internal/doctor"
)

func TestDoctorCodexCompatibility(t *testing.T) {
	for _, tc := range []struct {
		version, detail string
		status          doctor.Status
	}{
		{"0.155.1", "--no-daemon omitted (version rule)", doctor.Pass},
		{"0.156.0", "--no-daemon enabled for interactive launches (version rule)", doctor.Pass},
		{"0.999.0", "--no-daemon omitted (unknown)", doctor.Skip},
	} {
		p := compat.ForVersion(compat.Installation{Harness: "codex", Surface: compat.CLI, Version: tc.version})
		c := codexCompatibilityCheck(p)
		if c.Status != tc.status || !strings.Contains(c.Detail, tc.version) || !strings.Contains(c.Detail, tc.detail) {
			t.Fatalf("version %s: %+v", tc.version, c)
		}
	}
}
