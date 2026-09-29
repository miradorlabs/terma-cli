package compat

import "testing"

func TestVersionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    Support
	}{
		{"0.149.9", Unknown}, {"0.150.0", Unsupported}, {"0.155.0", Unsupported},
		{"0.155.1", Unsupported}, {"0.155.2", Unknown}, {"0.156.0", Supported},
		{"v0.156.0", Supported}, {"0.156.1", Supported}, {"0.157.1", Supported},
		{"0.157.2", Unknown}, {"0.158.0", Unknown}, {"1.0.0", Unknown},
		{"0.9.0", Unknown}, {"0.156.0-alpha.1", Unknown}, {"0.155.0-alpha.16", Unknown},
		{"0.156.0+custom", Unknown}, {"development", Unknown}, {"", Unknown},
	} {
		t.Run(tc.version, func(t *testing.T) {
			p := ForVersion(Installation{Harness: "codex", Surface: CLI, Version: tc.version})
			c := p.Capability(CodexNoDaemon)
			if c.Support != tc.want {
				t.Fatalf("got %+v, want %s", c, tc.want)
			}
			if c.Support != Unknown && (c.Source != "version rule" || c.Evidence == "") {
				t.Fatal("rule lost provenance")
			}
		})
	}
}

func TestParseVersion(t *testing.T) {
	for _, v := range []string{"0.156.0", "v0.156.0", "0.156.0-alpha.10", "0.156.0+vendor.2"} {
		if _, ok := ParseVersion(v); !ok {
			t.Errorf("rejected %q", v)
		}
	}
	for _, v := range []string{"0.156", "0", "01.156.0", "0.156.0-alpha.01", "0.156.0-", "codex-cli 0.156.0", "0.156.0junk"} {
		if _, ok := ParseVersion(v); ok {
			t.Errorf("accepted %q", v)
		}
	}
}

func TestRulesAreScopedToHarnessAndSurface(t *testing.T) {
	for _, i := range []Installation{
		{Harness: "codex", Surface: Desktop, Version: "0.156.0"},
		{Harness: "claude", Surface: CLI, Version: "0.156.0"},
		{Harness: "codex", Version: "0.156.0"},
	} {
		if c := ForVersion(i).Capability(CodexNoDaemon); c.Support != Unknown {
			t.Fatalf("CLI rule leaked into %+v", i)
		}
	}
}
