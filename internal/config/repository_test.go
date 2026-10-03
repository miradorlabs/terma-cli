package config

import "testing"

func TestPolicyAdmits(t *testing.T) {
	list := Policy{Mode: ModeRepo, Repositories: []string{"github.com/miradorlabs/mirador-platform", "GitHub.com/Acme/Sales", " gitlab.example.com/group/sub/app ", "github.com/acme", "github.com//web", "mirador-platform"}}
	for _, tc := range []struct {
		name string
		pol  Policy
		repo Repository
		want bool
	}{
		{"origin equals an entry", list, Repository{Origin: "github.com/miradorlabs/mirador-platform"}, true},
		{"case does not matter", list, Repository{Origin: "github.com/acme/sales"}, true},
		{"a subgroup path matches exactly", list, Repository{Origin: "gitlab.example.com/group/sub/app"}, true},
		{"a shorter path is another repository", list, Repository{Origin: "gitlab.example.com/group/sub"}, false},
		{"a longer path is another repository", list, Repository{Origin: "github.com/acme/sales/extra"}, false},
		{"a fork under another owner", list, Repository{Origin: "github.com/someone/mirador-platform"}, false},
		{"another host", list, Repository{Origin: "gitlab.com/miradorlabs/mirador-platform"}, false},
		{"an entry with one path segment matches nothing", list, Repository{Origin: "github.com/acme"}, false},
		{"an entry with an empty segment matches nothing", list, Repository{Origin: "github.com//web"}, false},
		{"no origin matches nothing", list, Repository{}, false},
		{"an empty list admits nothing", Policy{Mode: ModeRepo, Repositories: []string{}}, Repository{Origin: "github.com/acme/sales"}, false},
		{"no policy admits nothing", NoPolicy("", ""), Repository{Origin: "github.com/acme/sales"}, false},
		{"global mode admits everything", Policy{Mode: ModeGlobal}, Repository{}, true},
	} {
		if got := tc.pol.Admits(tc.repo); got != tc.want {
			t.Errorf("%s: Admits(%+v) = %v", tc.name, tc.repo, got)
		}
	}
}
