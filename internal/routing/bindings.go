package routing

import (
	"errors"
	"fmt"
)

// Binding is the developer's decision about an owner or a repository: bound to one
// organization, which alone decides about it, or refused to every organization.
type Binding struct {
	Organization string `json:"organization,omitempty"`
	Refused      bool   `json:"refused,omitempty"`
}

// AuthorityKind is who decides about a repository.
type AuthorityKind string

// Authorities, from the developer's decisions and the selected teams' entries.
const (
	// Free: no decision, and no selected organization's entry needs one. Personal may
	// collect it.
	Free AuthorityKind = "free"
	// Pending: no decision, and a selected organization's entry needs one. Records its
	// entries match are held until the developer decides.
	Pending AuthorityKind = "pending"
	// Bound: one organization decides, including not collecting it. Personal never does.
	Bound AuthorityKind = "bound"
	// RefusedAll: every organization's entries are skipped. Personal may collect it.
	RefusedAll AuthorityKind = "refused"
)

// Authority is who decides about one repository, and the key the decision is kept under.
type Authority struct {
	Kind AuthorityKind
	// Organization is the deciding organization when Kind is Bound.
	Organization string
	// Key is the owner or repository decided on, or for Pending the binding the matching
	// entry needs; "" when Free.
	Key string
}

// decided is the developer's explicit decision about a normalised repo: on the
// repository, else on its owner; Free when there is none. Route derives Pending from the
// selected teams' entries, never storing it, so an organization adding an entry never
// shadows a decision the developer made.
func (s State) decided(repo string) Authority {
	for _, key := range []string{repo, owner(repo)} {
		if b, ok := s.Bindings[key]; ok {
			if b.Refused {
				return Authority{Kind: RefusedAll, Key: key}
			}
			return Authority{Kind: Bound, Organization: b.Organization, Key: key}
		}
	}
	return Authority{Kind: Free}
}

// consents reports whether the binding e needs is made, to org: its owner's for a
// wildcard; its repository's, or its owner's, for an exact entry.
func (s State) consents(e Entry, org string) bool {
	keys := []string{e.Key()}
	if !e.wildcard {
		keys = append(keys, owner(e.key))
	}
	for _, key := range keys {
		if b, ok := s.Bindings[key]; ok && !b.Refused && b.Organization == org {
			return true
		}
	}
	return false
}

// boundToAnOrganization reports whether anything is bound to an organization, which keeps
// unknown records from personal: one could be that organization's.
func (s State) boundToAnOrganization() bool {
	for _, b := range s.Bindings {
		if !b.Refused {
			return true
		}
	}
	return false
}

// Bind binds an owner or a repository to organization. A repository decision against an
// owner already decided otherwise is made only with override, so the caller has named the
// conflict to the developer first. Nothing is bound to the personal organization: personal
// collects what no organization is bound to.
func (s *State) Bind(key, organization string, override bool) error {
	key, err := bindingKey(key)
	if err != nil {
		return err
	}
	if organization == "" {
		return errors.New("bind to which organization?")
	}
	if n := len(s.Teams); n > 0 && s.Teams[n-1].Personal && s.Teams[n-1].OrganizationID == organization {
		return errors.New("nothing is bound to the personal organization: it collects what no organization is bound to")
	}
	if b, ok := s.Bindings[owner(key)]; ok && validRepo(key) && !override && (b.Refused || b.Organization != organization) {
		return fmt.Errorf("%s is under %s, which is decided otherwise", key, owner(key))
	}
	s.set(key, Binding{Organization: organization})
	return nil
}

// Refuse refuses an owner or a repository to every organization.
func (s *State) Refuse(key string) error {
	key, err := bindingKey(key)
	if err != nil {
		return err
	}
	s.set(key, Binding{Refused: true})
	return nil
}

// Unbind removes the decision on key. Route then derives it as pending while a selected
// organization's entry needs it, and free otherwise; neither is stored.
func (s *State) Unbind(key string) error {
	key, err := bindingKey(key)
	if err != nil {
		return err
	}
	delete(s.Bindings, key)
	return nil
}

func (s *State) set(key string, b Binding) {
	if s.Bindings == nil {
		s.Bindings = map[string]Binding{}
	}
	s.Bindings[key] = b
}

// bindingKey normalises an owner (`host/owner`) or a repository (`host/owner/name`), in
// any form Normalize reads; a wildcard is never one, and an empty segment, a query or a
// fragment is refused rather than folded away.
func bindingKey(raw string) (string, error) {
	if key := Normalize(raw); !reinterpreted(raw) && (isOwnerKey(key) || validRepo(key)) {
		return key, nil
	}
	return "", fmt.Errorf("%q is neither an owner (host/owner) nor a repository (host/owner/name)", raw)
}
