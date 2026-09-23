package gitx

import (
	"strings"
	"testing"
)

// A remote is emitted as telemetry, so anything that could carry a secret has to
// be stripped before it leaves the machine.
func TestNormalizeRemoteStripsCredentials(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			name: "https with token",
			in:   "https://dawson:ghp_supersecrettoken@github.com/miradorlabs/terma-cli.git",
			want: "https://github.com/miradorlabs/terma-cli",
		},
		{
			name: "https with user only",
			in:   "https://dawson@github.com/miradorlabs/terma-cli",
			want: "https://github.com/miradorlabs/terma-cli",
		},
		{
			name: "scp form",
			in:   "git@github.com:miradorlabs/terma-cli.git",
			want: "https://github.com/miradorlabs/terma-cli",
		},
		{
			name: "ssh scheme",
			in:   "ssh://git@github.com/miradorlabs/terma-cli.git",
			want: "https://github.com/miradorlabs/terma-cli",
		},
		{
			name: "plain https",
			in:   "https://github.com/miradorlabs/terma-cli",
			want: "https://github.com/miradorlabs/terma-cli",
		},
		{
			name: "self-hosted",
			in:   "git@git.internal.example.com:team/repo.git",
			want: "https://git.internal.example.com/team/repo",
		},
		{
			name: "http is upgraded",
			in:   "http://github.com/o/r.git",
			want: "https://github.com/o/r",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeRemote(tc.in)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if strings.ContainsAny(got, "@") {
				t.Fatalf("userinfo survived normalisation: %q", got)
			}
			if strings.Contains(got, "ghp_") || strings.Contains(got, "supersecret") {
				t.Fatalf("credential survived normalisation: %q", got)
			}
		})
	}
}

// Anything that is not a browsable remote yields "", so nothing invents a link.
func TestNormalizeRemoteRejectsUnusable(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"/srv/git/repo.git",
		"file:///srv/git/repo.git",
		"../relative/path",
	} {
		if got := NormalizeRemote(in); got != "" {
			t.Fatalf("NormalizeRemote(%q) = %q, want \"\"", in, got)
		}
	}
}
