package config

import "testing"

func TestPolicyAdmits(t *testing.T) {
	t.Parallel()
	list := Policy{Mode: ModeRepo, Repositories: []string{"github.com/miradorlabs/mirador-platform", "GitHub.com/Acme/Sales", " gitlab.example.com/group/sub/app ", "github.com/acme", "github.com//web", "mirador-platform"}}
	entries := []string{"github.com/Acme/*", "gitlab.com/acme/platform/*", "bitbucket.org/*", "github.com/other/terma-*", "github.com/solo/api"}
	wild := Policy{Mode: ModeRepo, Repositories: entries, Wildcards: true}
	literal := Policy{Mode: ModeRepo, Repositories: entries}
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
		{"a 2.x wildcard admits a repository under its owner", wild, Repository{Origin: "github.com/acme/web"}, true},
		{"a 2.x wildcard ignores case", wild, Repository{Origin: "GitHub.com/ACME/Web"}, true},
		{"a 2.x wildcard after a subgroup admits below it", wild, Repository{Origin: "gitlab.com/acme/platform/sub/api"}, true},
		{"a 2.x wildcard is not its owner", wild, Repository{Origin: "github.com/acme"}, false},
		{"a 2.x wildcard is not a longer owner", wild, Repository{Origin: "github.com/acmeco/web"}, false},
		{"a 2.x wildcard is not above its subgroup", wild, Repository{Origin: "gitlab.com/acme/api"}, false},
		{"a 2.x wildcard is not another host", wild, Repository{Origin: "gitlab.com/acme/web"}, false},
		{"a 2.x wildcard on a host alone is refused", wild, Repository{Origin: "bitbucket.org/acme/web"}, false},
		{"a 2.x wildcard inside a name is refused", wild, Repository{Origin: "github.com/other/terma-cli"}, false},
		{"an exact 2.x entry is still exact", wild, Repository{Origin: "github.com/solo/api"}, true},
		{"a 1.x entry ending in /* is a literal that names no repository", literal, Repository{Origin: "github.com/acme/web"}, false},
		{"a 1.x entry ending in /* does not admit an origin spelled with *", literal, Repository{Origin: "github.com/acme/*"}, false},
		{"a clone URL entry reads as the repository it names", Policy{Mode: ModeRepo, Repositories: []string{"https://github.com/acme/clone.git"}}, Repository{Origin: "github.com/acme/clone"}, true},
	} {
		if got := tc.pol.Admits(tc.repo); got != tc.want {
			t.Errorf("%s: Admits(%+v) = %v", tc.name, tc.repo, got)
		}
	}
}
