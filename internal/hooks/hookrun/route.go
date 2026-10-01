package hookrun

import (
	"sync"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// Route is a project's routing record: the agents, surfaces and signals the developer
// routes, and the content they let through.
type Route = routing.Record

type routeRead struct {
	rec      Route
	recorded bool
	err      error
}

// Route is the project's routing record, read on first use and kept for the rest of the
// hook. An unreadable record is its error, never "not recorded": it may be the one that
// withholds content.
func (r *Repo) Route() (rec Route, recorded bool, err error) {
	if r.route == nil {
		r.route = sync.OnceValue(func() routeRead { return readRoute(r.ProjectID) })
	}
	rr := r.route()
	return rr.rec, rr.recorded, rr.err
}

func readRoute(projectID string) routeRead {
	rec, recorded, err := routing.LoadRecord(projectID)
	return routeRead{rec, recorded, err}
}

// ProjectPolicy is the collection policy that applies to the repository's project: its
// team's cached policy, else the organization's, else none.
func (e Env) ProjectPolicy(r *Repo) config.Policy {
	return routing.EffectivePolicy(e.Policy, r.ProjectID)
}

// RelayEnabled reports whether the local relay is set up on this machine.
func (Env) RelayEnabled() bool { return claim.Enabled() }

// Consent is what spooled conversation content (a reply, a thread's name) travels under,
// gathered fresh by whoever asks: a hook, or a delivery that predates a policy change.
type Consent struct {
	Route    Route
	Recorded bool
	// RouteErr is a routing record that exists and cannot be read.
	RouteErr error
	// Relay is the local relay set up on this machine.
	Relay  bool
	Global bool
}

// Consent is this repository's consent, with the record this hook already read.
func (r *Repo) Consent(global bool) Consent {
	rec, recorded, err := r.Route()
	return Consent{Route: rec, Recorded: recorded, RouteErr: err, Relay: claim.Enabled(), Global: global}
}

// ConsentFor is a project's consent as its files say now.
func ConsentFor(projectID string, global bool) Consent {
	rr := readRoute(projectID)
	return Consent{Route: rr.rec, Recorded: rr.recorded, RouteErr: rr.err, Relay: claim.Enabled(), Global: global}
}
