package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

type bindingChoice struct {
	key    string
	org    string
	refuse bool
}

// decideSetupBindings computes the binding decisions for target's listed repositories.
// Personal and global teams need no bindings; entries already covered in existing are
// skipped so existing developer decisions are never overwritten. Under non-interactive or
// -y mode (ask == false), safe defaults bind each needed entry key (exact repo for exact
// entries, owner for wildcards) to target.OrganizationID.
func decideSetupBindings(target routing.Team, orgLabel string, existing map[string]routing.Binding, ask bool) ([]bindingChoice, error) {
	if target.Personal || target.Policy.Global() || len(target.Policy.Repositories) == 0 {
		return nil, nil
	}
	var needed []routing.Entry
	seen := map[string]bool{}
	var owners []string
	seenOwner := map[string]bool{}
	anyWildcard := false
	for _, raw := range target.Policy.Repositories {
		e, ok := routing.ParseEntry(raw, target.Policy.Wildcards)
		if !ok || alreadyDecided(existing, e) || seen[e.Key()] {
			continue
		}
		seen[e.Key()] = true
		needed = append(needed, e)
		if e.Wildcard() {
			anyWildcard = true
		}
		owner := routing.Owner(e.Key())
		if !seenOwner[owner] {
			seenOwner[owner] = true
			owners = append(owners, owner)
		}
	}
	if len(needed) == 0 {
		return nil, nil
	}
	if !ask {
		out := make([]bindingChoice, 0, len(needed))
		for _, e := range needed {
			out = append(out, bindingChoice{key: e.Key(), org: target.OrganizationID})
		}
		return out, nil
	}
	if !anyWildcard {
		items := []prompt.Item{
			{Label: fmt.Sprintf("Bind just these %d repositor%s to %s", len(needed), pluralY(len(needed)), orgLabel), Selected: true},
			{Label: fmt.Sprintf("Bind whole owner (%s) to %s", strings.Join(owners, ", "), orgLabel)},
			{Label: "Decide later (hold matching sessions until bound)"},
		}
		choice, err := prompt.Choose("Bind repositories to "+orgLabel+"?", items)
		if errors.Is(err, prompt.ErrCancelled) {
			return nil, errCancelled
		}
		if err != nil {
			return nil, err
		}
		switch choice {
		case 0:
			out := make([]bindingChoice, 0, len(needed))
			for _, e := range needed {
				out = append(out, bindingChoice{key: e.Key(), org: target.OrganizationID})
			}
			return out, nil
		case 1:
			out := make([]bindingChoice, 0, len(owners))
			for _, o := range owners {
				out = append(out, bindingChoice{key: o, org: target.OrganizationID})
			}
			return out, nil
		default:
			return nil, nil
		}
	}
	items := []prompt.Item{
		{Label: fmt.Sprintf("Bind %s to %s", strings.Join(owners, ", "), orgLabel), Selected: true},
		{Label: "Refuse (do not collect these repositories for any organization)"},
		{Label: "Decide later (hold matching sessions until bound)"},
	}
	choice, err := prompt.Choose("Bind repositories to "+orgLabel+"?", items)
	if errors.Is(err, prompt.ErrCancelled) {
		return nil, errCancelled
	}
	if err != nil {
		return nil, err
	}
	switch choice {
	case 0:
		out := make([]bindingChoice, 0, len(needed))
		for _, e := range needed {
			out = append(out, bindingChoice{key: e.Key(), org: target.OrganizationID})
		}
		return out, nil
	case 1:
		out := make([]bindingChoice, 0, len(needed))
		for _, e := range needed {
			out = append(out, bindingChoice{key: e.Key(), refuse: true})
		}
		return out, nil
	default:
		return nil, nil
	}
}

func applySetupBindings(s *routing.State, target routing.Team, choices []bindingChoice) error {
	if target.Personal || len(choices) == 0 {
		return nil
	}
	for _, c := range choices {
		if _, decided := s.Bindings[c.key]; decided {
			continue
		}
		if config.IsRepositoryKey(c.key) {
			if _, ownerDecided := s.Bindings[routing.Owner(c.key)]; ownerDecided {
				continue
			}
		}
		if c.refuse {
			if err := s.Refuse(c.key); err != nil {
				return err
			}
		} else if err := s.Bind(c.key, c.org, false); err != nil {
			return err
		}
	}
	return nil
}

func alreadyDecided(bindings map[string]routing.Binding, e routing.Entry) bool {
	if len(bindings) == 0 {
		return false
	}
	if _, ok := bindings[e.Key()]; ok {
		return true
	}
	if !e.Wildcard() {
		_, ok := bindings[routing.Owner(e.Key())]
		return ok
	}
	return false
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
