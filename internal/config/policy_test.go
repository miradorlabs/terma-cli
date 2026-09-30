package config

import "testing"

func TestNormalizeRemote(t *testing.T) {
	for _, in := range []string{
		"git@github.com:miradorlabs/terma-cli.git",
		"ssh://git@github.com/miradorlabs/terma-cli",
		"https://github.com/miradorlabs/terma-cli.git",
		"https://someone@GitHub.com/miradorlabs/terma-cli/",
		"ssh://git@github.com:22/miradorlabs/terma-cli.git",
	} {
		if got := NormalizeRemote(in); got != "github.com/miradorlabs/terma-cli" {
			t.Errorf("NormalizeRemote(%q) = %q", in, got)
		}
	}
	if NormalizeRemote("  ") != "" {
		t.Fatal("an empty remote normalized to something")
	}
}

// Global mode files a known remote under its project, anything else under the default;
// repo mode files nothing.
func TestPolicyProjectFor(t *testing.T) {
	p := Policy{Mode: ModeGlobal, DefaultProjectID: "p-default", Remotes: map[string]string{"github.com/org/app": "p-app"}}
	if got := p.ProjectFor("git@github.com:org/app.git"); got != "p-app" {
		t.Fatalf("known remote: %q", got)
	}
	if got := p.ProjectFor("https://gitlab.com/me/side"); got != "p-default" {
		t.Fatalf("unknown remote: %q", got)
	}
	if got := p.ProjectFor(""); got != "p-default" {
		t.Fatalf("no remote: %q", got)
	}
	p.Mode = ModeRepo
	if got := p.ProjectFor("git@github.com:org/app.git"); got != "" {
		t.Fatalf("repo mode placed a session: %q", got)
	}
}
