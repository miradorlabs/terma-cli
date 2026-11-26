package routing

import "slices"

// Overlap is an entry two selected teams' policies both cover: Route sends what it
// matches to Winner, which comes first in Order, and never to Other.
type Overlap struct {
	// Entry is the narrower of the two entries, which is what both cover.
	Entry         string
	Winner, Other Team
}

// Order is the order Route tries s's teams in: the organization teams that can collect,
// most recently updated policy first (equal or unknown ages keep the order ticked), then
// personal. A refused team is passed over, so it is left out.
func Order(s State) []Team {
	var out []Team
	for _, t := range s.Teams {
		if !t.Personal && !t.refused() {
			out = append(out, t)
		}
	}
	slices.SortStableFunc(out, func(a, b Team) int {
		switch {
		case newer(a.Policy, b.Policy):
			return -1
		case newer(b.Policy, a.Policy):
			return 1
		}
		return 0
	})
	if n := len(s.Teams); n > 0 && s.Teams[n-1].Personal {
		out = append(out, s.Teams[n-1])
	}
	return out
}

// Overlaps lists every pair of entries in which two selected teams, in one organization or
// two, claim the same repositories, with the team Route gives them to. A refused team claims nothing,
// and a pair whose winner itself loses the entry to a team earlier in Order is left out.
func Overlaps(s State) []Overlap {
	order := Order(s)
	rank := func(t Team) int {
		return slices.IndexFunc(order, func(o Team) bool { return o.EnrollmentID == t.EnrollmentID })
	}
	var out []Overlap
	for i, a := range s.Teams {
		for j := i + 1; j < len(s.Teams); j++ {
			b := s.Teams[j]
			if a.Personal || b.Personal || a.refused() || b.refused() {
				continue
			}
			winner, other := a, b
			if rank(b) < rank(a) {
				winner, other = b, a
			}
			for _, ea := range entries(a) {
				for _, eb := range entries(b) {
					if !ea.Overlaps(eb) {
						continue
					}
					narrow := ea
					if eb.Wildcard() == ea.Wildcard() && eb.Fixed() > ea.Fixed() || ea.Wildcard() && !eb.Wildcard() {
						narrow = eb
					}
					o := Overlap{Entry: narrow.Raw, Winner: winner, Other: other}
					if !outranked(order[:rank(winner)], narrow) && !slices.ContainsFunc(out, o.same) {
						out = append(out, o)
					}
				}
			}
		}
	}
	return out
}

// same reports whether o and v name one entry between the same two teams.
func (o Overlap) same(v Overlap) bool {
	return o.Entry == v.Entry && o.Winner.EnrollmentID == v.Winner.EnrollmentID && o.Other.EnrollmentID == v.Other.EnrollmentID
}

// outranked reports whether a team Route tries first covers all of e, so the winner
// collects none of it.
func outranked(first []Team, e Entry) bool {
	for _, t := range first {
		for _, te := range entries(t) {
			if te.Contains(e) {
				return true
			}
		}
	}
	return false
}

// entries are t's parsed repository entries; none for a policy that lists none.
func entries(t Team) []Entry {
	if t.Policy.Global() || t.Policy.Unset {
		return nil
	}
	var out []Entry
	for _, raw := range t.Policy.Repositories {
		if e, ok := ParseEntry(raw, t.Policy.Wildcards); ok {
			out = append(out, e)
		}
	}
	return out
}
