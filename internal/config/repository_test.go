package config

import "testing"

func TestPolicyAdmits(t *testing.T) {
	list := Policy{Mode: ModeRepo, Folders: []string{"mirador-platform", "Acme/Sales", " owner/repo/ "}}
	for _, tc := range []struct {
		name string
		pol  Policy
		repo Repository
		want bool
	}{
		{"a name matches origin's repository name", list, Repository{Names: []string{"checkout-a", "mirador-platform"}, Path: "miradorlabs/mirador-platform"}, true},
		{"a name matches the folder whatever the remote", list, Repository{Names: []string{"mirador-platform", "other"}, Path: "acme/other"}, true},
		{"a name matches a parent folder outside Git", list, Repository{Names: []string{"notes", "mirador-platform"}}, true},
		{"case does not matter", list, Repository{Names: []string{"MIRADOR-PLATFORM"}}, true},
		{"owner/name matches origin's path", list, Repository{Names: []string{"sales"}, Path: "acme/sales"}, true},
		{"owner/name never matches a folder name", list, Repository{Names: []string{"sales"}}, false},
		{"the same name under another owner", list, Repository{Names: []string{"sales"}, Path: "other/sales"}, false},
		{"entries are trimmed", list, Repository{Names: []string{"repo"}, Path: "owner/repo"}, true},
		{"unlisted", list, Repository{Names: []string{"web"}, Path: "acme/web"}, false},
		{"no identity matches nothing", list, Repository{}, false},
		{"an empty list admits nothing", Policy{Mode: ModeRepo, Folders: []string{}}, Repository{Names: []string{"sales"}}, false},
		{"no policy admits nothing", NoPolicy("", ""), Repository{Names: []string{"sales"}}, false},
		{"global mode admits everything", Policy{Mode: ModeGlobal}, Repository{}, true},
	} {
		if got := tc.pol.Admits(tc.repo); got != tc.want {
			t.Errorf("%s: Admits(%+v) = %v", tc.name, tc.repo, got)
		}
	}
}
