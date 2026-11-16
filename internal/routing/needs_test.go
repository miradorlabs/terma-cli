package routing

import "testing"

// The binding rules `terma teams`, `--bind` and doctor share: a key two organizations need
// is a conflict; an owner settles another organization's repository under it unless that
// repository is decided with it; binding an owner whole takes what another needs under it;
// and a repository under an owner another organization needs is an exception to it.
func TestNeedsRules(t *testing.T) {
	ix := IndexNeeds([]Need{
		{"github.com/acme", "a"},
		{"github.com/acme/api", "b"},
		{"github.com/acme/web", "a"},
		{"github.com/shared/x", "a"},
		{"github.com/shared/x", "b"},
		{"github.com/shared/x", "b"},
		{"github.com/solo/one", "a"},
	})
	if !ix.Conflict("github.com/shared/x") || ix.Conflict("github.com/acme") || ix.Conflict("github.com/acme/api") {
		t.Error("conflicts: the same key, two organizations; never an owner and a repository under it")
	}
	if got := ix.Organizations("github.com/shared/x"); len(got) != 2 {
		t.Errorf("organizations = %v, each once", got)
	}
	if n, ok := ix.Settles("github.com/acme", "a", nil); !ok || n != (Need{"github.com/acme/api", "b"}) {
		t.Errorf("settles = %+v, %v", n, ok)
	}
	if _, ok := ix.Settles("github.com/acme", "a", func(k string) bool { return k == "github.com/acme/api" }); ok {
		t.Error("a repository decided with the owner is not settled by it")
	}
	if n, ok := ix.Settles("github.com/acme", "b", nil); !ok || n != (Need{"github.com/acme/web", "a"}) {
		t.Errorf("bound to b, the owner settles a's repository under it: %+v, %v", n, ok)
	}
	if _, ok := ix.Settles("github.com/acme/api", "a", nil); ok {
		t.Error("a repository settles nothing")
	}
	if !ix.Shared("github.com/acme", "a") || !ix.Shared("github.com/acme", "b") || ix.Shared("github.com/solo", "a") {
		t.Error("shared: another organization needs the owner or a repository under it")
	}
	if org, ok := ix.OwnerElsewhere("github.com/acme/api", "b"); !ok || org != "a" {
		t.Errorf("exception: %q, %v", org, ok)
	}
	if _, ok := ix.OwnerElsewhere("github.com/acme/web", "a"); ok {
		t.Error("its own organization's owner makes no exception")
	}
}
