package routing

import (
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// A queued record goes to its own team or nowhere: the plan's recheck table, row by row.
func TestRecheckNeverRedirects(t *testing.T) {
	t.Parallel()
	acme := func(s *State) Destination {
		return Destination{EnrollmentID: enrollment(t, s, "acme-platform"), Location: repo("github.com/acme/api")}
	}
	verdict := func(s *State, d Destination) Verdict {
		v, _ := NewRouter(*s).Recheck(d, now)
		return v
	}
	cases := []struct {
		name   string
		change func(*State)
		want   Verdict
	}{
		{"still selected, Active and covered", func(*State) {}, SendNow},
		{"expired", func(s *State) { s.Teams[0].Policy.FetchedAt = now.Add(-config.MaxPolicyAge - time.Hour) }, Wait},
		{"signing in", func(s *State) { s.Teams[0].SignInNeeded = true }, Wait},
		{"suspended", func(s *State) { s.Teams[0].Refusals = 1 }, Wait},
		{"refused", func(s *State) { s.Teams[0].Refusals = 2 }, Discard},
		{"its list no longer covers it", func(s *State) { s.Teams[0].Policy.Repositories = []string{"github.com/acme/web"} }, Discard},
		{"its owner was refused, so personal would now collect it", func(s *State) { must(t, s.Refuse("github.com/acme")) }, Discard},
		{"unticked", func(s *State) { s.Deselect(s.Teams[0].EnrollmentID) }, Discard},
	}
	for _, c := range cases {
		s := fixture(t)
		d := acme(s)
		c.change(s)
		if got := verdict(s, d); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}

	held := fixture(t)
	held.Teams[0].Policy.Repositories = append(held.Teams[0].Policy.Repositories, "github.com/torvalds/linux")
	d := Destination{EnrollmentID: enrollment(t, held, "acme-platform"), Location: repo("github.com/torvalds/linux")}
	if got := verdict(held, d); got != Wait {
		t.Fatalf("a record held for a pending binding: %s, want wait", got)
	}
	must(t, held.Bind("github.com/torvalds/linux", "acme", false))
	if got := verdict(held, d); got != SendNow {
		t.Fatalf("once bound to the holding organization: %s, want send", got)
	}
	refused := fixture(t)
	refused.Teams[0].Policy.Repositories = append(refused.Teams[0].Policy.Repositories, "github.com/torvalds/linux")
	must(t, refused.Refuse("github.com/torvalds/linux"))
	if got := verdict(refused, d); got != Discard {
		t.Fatalf("once refused, a record held for it went %s, want drop", got)
	}
	if got := verdict(fixture(t), Destination{EnrollmentID: "never-selected", Location: repo("github.com/acme/api")}); got != Discard {
		t.Fatalf("an unknown enrollment: %s", got)
	}
}
