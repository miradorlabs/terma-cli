package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// On a migrated machine the profile's routing reaches the handler and the claim that
// follows it, and a claimed checkout goes to ClaimedRouted with that routing, in Claimed's
// place: the repository's commit hooks follow routing, not a team's policy.
func TestARoutedProfileRoutesTheHookAndWhatFollows(t *testing.T) {
	t.Parallel()
	root := hookruntest.RepoAt(t, "github.com/acme/api")
	h := newHarness(t)
	rt := hookrun.NewRouting(hookruntest.RoutedProfile, hookruntest.TwoOrganizations(t))
	h.deps.Agents = agents.New(reading{fake{ran: &h.ran}})
	h.deps.Profile = func() Profile { return Profile{Policy: config.NoPolicy("", ""), Routing: rt} }
	var routedIn string
	var routedWith *hookrun.Routing
	h.deps.ClaimedRouted = func(cwd string, r *hookrun.Routing) { routedIn, routedWith = cwd, r }
	r := request("fake-edit")
	r.Cwd = t.TempDir()
	r.Stdin = strings.NewReader(`{"session_id":"routed-s1","cwd":"` + hookruntest.InJSON(root) + `"}`)
	Run(context.Background(), h.deps, r)
	if routedIn != root || routedWith != rt || h.claims != 0 {
		t.Fatalf("ClaimedRouted(%q, %p), Claimed %d times; want the payload's checkout with the profile's routing", routedIn, routedWith, h.claims)
	}
	if c, ok := claim.Read(h.deps.StateDir, "routed-s1", time.Now()); !ok || c.Kind != claim.KindRepository || c.ProjectID != "" {
		t.Fatalf("placement %+v, %v: the session is placed by location alone", c, ok)
	}
}
