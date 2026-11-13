package cli

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/routing"
)

// resolveBindingFlags turns --bind, --refuse and --unbind into decisions. A repository
// bound against its owner's decision names the conflict and asks first (--yes confirms it
// without a terminal). Binding an owner lists the repository decisions under it, which the
// developer keeps or removes.
func resolveBindingFlags(cmd *cobra.Command, st routing.State, f teamsFlags, ask bool, names teamNames, list func() teamsView, ch *teamsChange) error {
	for _, raw := range f.binds {
		keyRaw, orgRaw, ok := strings.Cut(raw, "=")
		if !ok || strings.TrimSpace(keyRaw) == "" || strings.TrimSpace(orgRaw) == "" {
			return fmt.Errorf("--bind %q: want <owner-or-repository>=<organization>", raw)
		}
		key, err := bindingKeyOf(keyRaw)
		if err != nil {
			return err
		}
		org, orgName, err := resolveOrg(strings.TrimSpace(orgRaw), names, list)
		if err != nil {
			return err
		}
		op := bindingOp{key: key, org: org}
		if owner := routing.Owner(key); owner != "" {
			if b, ok := st.Bindings[owner]; ok && (b.Refused || b.Organization != org) {
				conflict := owner + " is " + bindingWords(b, names) + ": bind " + key + " to " + cmp.Or(orgName, org) + " against it?"
				if err := confirmConflict(cmd, conflict, ask, f.assumeYes); err != nil {
					return err
				}
				op.override = true
			}
		} else if err := keepRepositoryDecisions(cmd, st, key, org, orgName, names, ask, ch); err != nil {
			return err
		}
		ch.ops = append(ch.ops, op)
		ch.changes = append(ch.changes, describeOp(op, orgName))
	}
	for _, raw := range f.refuses {
		key, err := bindingKeyOf(raw)
		if err != nil {
			return err
		}
		op := bindingOp{key: key, refuse: true}
		ch.ops = append(ch.ops, op)
		ch.changes = append(ch.changes, describeOp(op, ""))
	}
	for _, raw := range f.unbinds {
		key, err := bindingKeyOf(raw)
		if err != nil {
			return err
		}
		if _, ok := st.Bindings[key]; !ok {
			return fmt.Errorf("%s has no decision to remove", key)
		}
		ch.ops = append(ch.ops, bindingOp{key: key})
		after := st
		after.Bindings = maps.Clone(st.Bindings)
		delete(after.Bindings, key)
		what := "free: personal may collect it"
		if pendingKey(after, key) {
			what = "pending: a ticked organization's entry needs it, so what it matches is held until you decide"
		}
		ch.changes = append(ch.changes, "unbound "+key+"; it is now "+what)
	}
	return nil
}

// bindingKeyOf is raw as a binding key: an owner or a repository, normalised; a wildcard or
// anything else is refused.
func bindingKeyOf(raw string) (string, error) {
	var probe routing.State
	if err := probe.Refuse(raw); err != nil {
		return "", err
	}
	for k := range probe.Bindings {
		return k, nil
	}
	return "", fmt.Errorf("%q is neither an owner nor a repository", raw)
}

// pendingKey reports whether a ticked organization's entry needs key with no decision in st.
func pendingKey(st routing.State, key string) bool {
	for _, p := range pendingBindings(st) {
		if p.Key == key {
			return true
		}
	}
	return false
}

// bindingWords says what b decides, naming its organization.
func bindingWords(b routing.Binding, names teamNames) string {
	if b.Refused {
		return "refused to every organization"
	}
	return "bound to " + cmp.Or(names.Orgs[b.Organization], b.Organization)
}

// confirmConflict asks before a decision against an owner's: on a terminal, else only
// --yes makes it.
func confirmConflict(cmd *cobra.Command, question string, ask, assumeYes bool) error {
	switch {
	case assumeYes:
		return nil
	case !ask:
		return errors.New(question + " Pass --yes to make it")
	}
	ok, err := confirmDefault(cmd, question, false)
	if err != nil {
		return err
	}
	if !ok {
		return errCancelled
	}
	return nil
}

// keepRepositoryDecisions lists the repository decisions under owner, which is being bound
// to org, that decide otherwise; on a terminal the developer keeps or removes each, and
// without one each is kept and said.
func keepRepositoryDecisions(cmd *cobra.Command, st routing.State, owner, org, orgName string, names teamNames, ask bool, ch *teamsChange) error {
	for _, k := range slices.Sorted(maps.Keys(st.Bindings)) {
		b := st.Bindings[k]
		if routing.Owner(k) != owner || !b.Refused && b.Organization == org {
			continue
		}
		keep := true
		if ask {
			var err error
			if keep, err = confirmDefault(cmd, fmt.Sprintf("%s is %s. Keep that decision under %s bound to %s?", k, bindingWords(b, names), owner, cmp.Or(orgName, org)), true); err != nil {
				return err
			}
		}
		if keep {
			ch.warnings = append(ch.warnings, "kept "+k+" "+bindingWords(b, names)+", under "+owner)
			continue
		}
		op := bindingOp{key: k}
		ch.ops = append(ch.ops, op)
		ch.changes = append(ch.changes, describeOp(op, ""))
	}
	return nil
}

// resolveOrg is the organization ref names, by id or name: from the names `terma teams`
// last listed, else from the organizations listed now.
func resolveOrg(ref string, names teamNames, list func() teamsView) (id, name string, err error) {
	if parseOrgRef(ref).ID != "" {
		return ref, names.Orgs[ref], nil
	}
	for oid, oname := range names.Orgs {
		if strings.EqualFold(oname, ref) {
			return oid, oname, nil
		}
	}
	v := list()
	orgs := make([]organization, 0, len(v.Organizations))
	for _, o := range v.Organizations {
		orgs = append(orgs, organization{ID: o.ID, Name: o.Name})
	}
	o, err := matchOrganization(orgs, ref)
	if err != nil {
		return "", "", err
	}
	return o.ID, o.Name, nil
}
