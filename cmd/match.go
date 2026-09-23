package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// matchKind says how to read one kind of named thing — a project, an organization —
// and what to call it when nothing, or more than one thing, matches.
type matchKind[T any] struct {
	noun string
	// list is the command that prints the candidates, quoted the way a message
	// quotes it, so the "no match" error can say where to look.
	list string
	// title heads the picker.
	title string
	id    func(T) string
	name  func(T) string
}

var projectKind = matchKind[project]{
	noun:  "project",
	list:  "`terma project list`",
	title: "Select a project:",
	id:    func(p project) string { return p.ID },
	name:  func(p project) string { return p.Name },
}

var organizationKind = matchKind[organization]{
	noun:  "organization",
	list:  "`terma org list`",
	title: "Select an organization:",
	id:    func(o organization) string { return o.ID },
	name:  func(o organization) string { return o.Name },
}

// labels omits redundant IDs while keeping identically named choices distinct.
func (k matchKind[T]) labels(items []T) map[string]string {
	counts := map[string]int{}
	for _, item := range items {
		counts[strings.ToLower(k.name(item))]++
	}
	labels := map[string]string{}
	for _, item := range items {
		name, id := k.name(item), k.id(item)
		label := nameOrID(name, id)
		if name != "" && counts[strings.ToLower(name)] > 1 {
			label = fmt.Sprintf("%s (%s)", name, id)
		}
		labels[id] = label
	}
	return labels
}

// index resolves query against ids first, then exact names, then a unique
// case-insensitive prefix. An ambiguous prefix is an error rather than a guess:
// silently picking one of several would send reads, or a sign-in, somewhere the
// user did not intend.
func (k matchKind[T]) index(items []T, query string) (int, error) {
	query = strings.TrimSpace(query)
	for i := range items {
		if k.id(items[i]) == query {
			return i, nil
		}
	}
	for i := range items {
		if strings.EqualFold(k.name(items[i]), query) {
			return i, nil
		}
	}

	var matches []int
	lower := strings.ToLower(query)
	for i := range items {
		if strings.HasPrefix(strings.ToLower(k.name(items[i])), lower) {
			matches = append(matches, i)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return -1, fmt.Errorf("no %s matches %q — run %s", k.noun, query, k.list)
	default:
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = k.name(items[m])
		}
		return -1, fmt.Errorf("%q matches several %ss (%s) — use the full name or the id", query, k.noun, strings.Join(names, ", "))
	}
}

func (k matchKind[T]) match(items []T, query string) (*T, error) {
	i, err := k.index(items, query)
	if err != nil {
		return nil, err
	}
	return &items[i], nil
}

// pick prompts for one of items on a terminal. A typed name resolves exactly as the
// command's argument would, so the picker and the argument agree on what a string
// means.
func (k matchKind[T]) pick(cmd *cobra.Command, items []T, row func(T) pickRow) (*T, error) {
	rows := make([]pickRow, 0, len(items))
	for _, item := range items {
		rows = append(rows, row(item))
	}
	i, err := pick(cmd, k.title, rows, func(answer string) (int, error) {
		return k.index(items, answer)
	})
	if err != nil {
		return nil, err
	}
	return &items[i], nil
}
