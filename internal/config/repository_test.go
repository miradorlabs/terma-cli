package config

import "testing"

func TestPolicyAdmits(t *testing.T) {
	list := Policy{Mode: ModeRepo, Repositories: []string{"mirador-platform", "Acme/Sales", " owner/repo/ "}}
	sales := Repository{Name: "sales", Path: "acme/sales"}
	for _, tc := range []struct {
		name string
		pol  Policy
		repo Repository
		want bool
	}{
		{"a name matches the remote's name", list, Repository{Name: "mirador-platform", Path: "miradorlabs/mirador-platform"}, true},
		{"a name matches a remote-less folder", list, Repository{Name: "mirador-platform"}, true},
		{"case does not matter", list, Repository{Name: "MIRADOR-PLATFORM"}, true},
		{"owner/name matches the remote's path", list, sales, true},
		{"owner/name never matches a folder name", list, Repository{Name: "sales"}, false},
		{"the same name under another owner", list, Repository{Name: "sales", Path: "other/sales"}, false},
		{"entries are trimmed", list, Repository{Name: "repo", Path: "owner/repo"}, true},
		{"unlisted", list, Repository{Name: "web", Path: "acme/web"}, false},
		{"no identity matches nothing", list, Repository{}, false},
		{"an empty list admits nothing", Policy{Mode: ModeRepo, Repositories: []string{}}, sales, false},
		{"no policy admits nothing", NoPolicy("", ""), sales, false},
		{"global mode admits everything", Policy{Mode: ModeGlobal}, Repository{}, true},
	} {
		if got := tc.pol.Admits(tc.repo); got != tc.want {
			t.Errorf("%s: Admits(%+v) = %v", tc.name, tc.repo, got)
		}
	}
}
