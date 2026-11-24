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

var errPersonalBinding = errors.New("a personal organization cannot be bound: unbind or refuse so personal may collect it")

func resolveBindingFlags(cmd *cobra.Command, planned *routing.State, f teamsFlags, ask bool, names *teamNames, list func() teamsView, ch *teamsChange) error {
	for _, raw := range f.binds {
		keyRaw, orgRaw, ok := strings.Cut(raw, "=")
		if !ok || strings.TrimSpace(keyRaw) == "" || strings.TrimSpace(orgRaw) == "" {
			return fmt.Errorf("--bind %q: want <owner-or-repository>=<organization>", raw)
		}
		key, err := bindingKeyOf(keyRaw)
		if err != nil {
			return err
		}
		org, orgName, err := resolveOrg(strings.TrimSpace(orgRaw), *names, list)
		if err != nil {
			return err
		}
		op := bindingOp{key: key, org: org}
		if owner := routing.Owner(key); owner != "" {
			if b, ok := planned.Bindings[owner]; ok && (b.Refused || b.Organization != org) {
				conflict := owner + " is " + bindingWords(b, *names) + ": bind " + key + " to " + cmp.Or(orgName, org) + " against it?"
				if err := confirmConflict(cmd, conflict, ask, f.assumeYes); err != nil {
					return err
				}
				op.override = true
			}
		} else if err := keepRepositoryDecisions(cmd, planned, key, org, orgName, false, *names, ask, &ch.ops, &ch.changes, &ch.warnings); err != nil {
			return err
		}
		if err := op.apply(planned); err != nil {
			return err
		}
		ch.ops, ch.changes = append(ch.ops, op), append(ch.changes, describeOp(op, orgName))
	}
	for _, raw := range f.refuses {
		key, err := bindingKeyOf(raw)
		if err != nil {
			return err
		}
		if routing.Owner(key) == "" {
			if err := keepRepositoryDecisions(cmd, planned, key, "", "", true, *names, ask, &ch.ops, &ch.changes, &ch.warnings); err != nil {
				return err
			}
		}
		op := bindingOp{key: key, refuse: true}
		if err := op.apply(planned); err != nil {
			return err
		}
		ch.ops, ch.changes = append(ch.ops, op), append(ch.changes, describeOp(op, ""))
	}
	for _, raw := range f.unbinds {
		key, err := bindingKeyOf(raw)
		if err != nil {
			return err
		}
		if _, ok := planned.Bindings[key]; !ok {
			return fmt.Errorf("%s has no decision to remove", key)
		}
		op := bindingOp{key: key}
		if err := op.apply(planned); err != nil {
			return err
		}
		ch.ops = append(ch.ops, op)
		ch.changes = append(ch.changes, "unbound "+key+"; it is now "+unboundStatus(*planned, key, *names))
	}
	return nil
}

func unboundStatus(st routing.State, key string, names teamNames) string {
	if owner := routing.Owner(key); owner != "" {
		if b, ok := st.Bindings[owner]; ok {
			return "under " + owner + " (" + bindingWords(b, names) + ")"
		}
	}
	if slices.ContainsFunc(pendingBindings(st), func(p pendingRow) bool { return p.Key == key }) {
		return "pending: a ticked organization's entry needs it, so what it matches is held until you decide"
	}
	return "free: personal may collect it"
}

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

func bindingWords(b routing.Binding, names teamNames) string {
	if b.Refused {
		return "refused to every organization"
	}
	return "bound to " + cmp.Or(names.Orgs[b.Organization], b.Organization)
}

func confirmConflict(cmd *cobra.Command, question string, ask, assumeYes bool) error {
	if assumeYes {
		return nil
	}
	if !ask {
		return errors.New(question + " Pass --yes to make it")
	}
	ok, err := confirmDefault(cmd, question, false)
	if err != nil || ok {
		return err
	}
	return errCancelled
}

func keepRepositoryDecisions(cmd *cobra.Command, planned *routing.State, owner, org, orgName string, refuse bool, names teamNames, ask bool, ops *[]bindingOp, changes, warnings *[]string) error {
	ownerAction := "bound to " + cmp.Or(orgName, org)
	if refuse {
		ownerAction = "refused to every organization"
	}
	for _, k := range slices.Sorted(maps.Keys(planned.Bindings)) {
		b := planned.Bindings[k]
		if routing.Owner(k) != owner || (refuse && b.Refused) || (!refuse && !b.Refused && b.Organization == org) {
			continue
		}
		keep := true
		if ask {
			var err error
			if keep, err = confirmDefault(cmd, fmt.Sprintf("%s is %s. Keep that decision under %s %s?", k, bindingWords(b, names), owner, ownerAction), true); err != nil {
				return err
			}
		}
		if keep {
			*warnings = append(*warnings, "kept "+k+" "+bindingWords(b, names)+", under "+owner)
			continue
		}
		op := bindingOp{key: k}
		if err := op.apply(planned); err != nil {
			return err
		}
		*ops = append(*ops, op)
		if changes != nil {
			*changes = append(*changes, describeOp(op, ""))
		}
	}
	return nil
}

func resolveOrg(ref string, names teamNames, list func() teamsView) (id, name string, err error) {
	v := list()
	if orgID := strings.ToLower(parseOrgRef(ref).ID); orgID != "" {
		if i := slices.IndexFunc(v.Organizations, func(o orgRow) bool { return strings.EqualFold(o.ID, orgID) }); i >= 0 {
			if v.Organizations[i].Type == "personal" {
				return "", "", errPersonalBinding
			}
			return v.Organizations[i].ID, cmp.Or(v.Organizations[i].Name, names.Orgs[v.Organizations[i].ID]), nil
		}
		return orgID, names.Orgs[orgID], nil
	}
	if v.Unlisted != "" {
		return "", "", fmt.Errorf("cannot resolve organization name %q while organizations cannot be listed (%s); pass the organization id", ref, v.Unlisted)
	}
	orgs, personalMatch := make([]organization, 0, len(v.Organizations)), false
	for _, o := range v.Organizations {
		if o.Type == "personal" {
			personalMatch = personalMatch || strings.EqualFold(o.Name, strings.TrimSpace(ref))
		} else if !o.Inaccessible {
			orgs = append(orgs, organization{ID: o.ID, Name: o.Name})
		}
	}
	idx, err := matchUniqueRef(organizationKind, orgs, ref, func(o organization) string {
		return fmt.Sprintf("%s (%s)", cmp.Or(o.Name, o.ID), o.ID)
	})
	if err != nil {
		if personalMatch {
			return "", "", errPersonalBinding
		}
		return "", "", err
	}
	return orgs[idx].ID, orgs[idx].Name, nil
}
