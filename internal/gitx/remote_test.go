package gitx

import (
	"strings"
	"testing"
)

// A remote leaves the machine as telemetry, so anything that could carry a secret is stripped.
func TestNormalizeRemoteStripsCredentials(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{
			name: "https with token",
			in:   "https://alex:ghp_supersecrettoken@github.com/miradorlabs/terma-cli.git",
			want: "https://github.com/miradorlabs/terma-cli",
		},
		{
			name: "https with user only",
			in:   "https://alex@github.com/miradorlabs/terma-cli",
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
	t.Parallel()
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

// A remote in any of git's URL forms names its repository as host/path; nothing local does.
func TestRepositoryID(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"git@github.com:acme/repo.git":                         "github.com/acme/repo",
		"git@GitHub.com:Acme/Repo":                             "github.com/Acme/Repo",
		"https://github.com/acme/repo":                         "github.com/acme/repo",
		"https://alex:tok@GitHub.com:443/acme/repo.git":        "github.com/acme/repo",
		"https://github.com/acme/repo.git/":                    "github.com/acme/repo",
		"https://github.com/acme/repo/":                        "github.com/acme/repo",
		"http://github.com/acme/repo.git":                      "github.com/acme/repo",
		"ssh://git@github.com/acme/repo.git":                   "github.com/acme/repo",
		"ssh://git@gitlab.example.com:2222/group/sub/repo.git": "gitlab.example.com/group/sub/repo",
		"git://git.example.com/acme/repo.git":                  "git.example.com/acme/repo",
		"git@github-work:acme/api.git":                         "github-work/acme/api",
		"  git@github.com:acme/repo.git  ":                     "github.com/acme/repo",
		"https://github.com":                                   "",
		"https://github.com/":                                  "",
		"github.com:acme/repo":                                 "",
		"/srv/git/repo.git":                                    "",
		"../relative/repo":                                     "",
		"file:///srv/git/repo.git":                             "",
		"file:///C:/repos/app.git":                             "",
		`C:\repos\billing`:                                     "",
		"C:/repos/billing":                                     "",
		`\\server\share\tools.git`:                             "",
		"":                                                     "",
	} {
		if got := RepositoryID(in); got != want {
			t.Errorf("RepositoryID(%q) = %q, want %q", in, got, want)
		}
	}
}
