package main

import (
	"strings"

	"github.com/miradorlabs/terma-cli/e2e/sourcelink"
)

// Source links: where a harness's public source names what a change is about, at the build it
// is in. A thing named (a tool, a field, a beta, a flag, a model) is found by its name; a
// text's change (a prompt, a tool's description) by the lines it adds, in the newer build, or
// by those it removes, in the older one. A removal is looked for in the build before, and says
// it is gone. A harness whose source is not public, or a build whose source cannot be read,
// leaves its changes unlinked.

// maxRefs bounds the lines a change links, as compatgen's findings do.
const maxRefs = 2

// sourceLinker links through a source library, one tarball a build.
type sourceLinker struct{ lib *sourcelink.Library }

// newLinker is the run's linker: none unless asked for.
func newLinker(enabled bool) *sourceLinker {
	if !enabled {
		return nil
	}
	// No folder: each build's tarball is kept for the run alone, so a backfill over a hundred
	// builds does not keep them all.
	return &sourceLinker{lib: sourcelink.New("")}
}

func (l *sourceLinker) close() {
	if l != nil {
		l.lib.Close()
	}
}

// unlinkable are the categories whose changes no source names: what terma concluded, and what
// the build's bytes or a scenario measured.
var unlinkable = map[string]bool{"support": true, "binary": true, "behavior": true, "storage": true}

func (l *sourceLinker) link(harness, from, to string, c *Change) *SourceNote {
	if l == nil || !sourcelink.Has(harness) || unlinkable[c.Category] {
		return nil
	}
	version, gone := to, c.Op == "removed"
	if gone {
		version = from
	}
	var refs []sourcelink.Ref
	if c.TextDiff != nil {
		added, removed := diffSides(*c.TextDiff)
		text := added
		if strings.TrimSpace(text) == "" {
			text, version = removed, from
		}
		if strings.TrimSpace(text) == "" {
			return nil
		}
		found, err := l.lib.FindText(harness, version, text)
		if err != nil {
			return nil
		}
		refs = found
	} else {
		needle := needleOf(c)
		if needle == "" {
			return nil
		}
		found, err := l.lib.Find(harness, version, []string{needle})
		if err != nil {
			return nil
		}
		refs = found[needle]
		if len(refs) == 0 && strings.HasPrefix(needle, "--") {
			// A flag is often declared by its long name alone.
			bare := strings.TrimPrefix(needle, "--")
			if found, err := l.lib.Find(harness, version, []string{bare}); err == nil {
				refs = found[bare]
			}
		}
	}
	if len(refs) == 0 {
		return nil
	}
	if len(refs) > maxRefs {
		refs = refs[:maxRefs]
	}
	note := &SourceNote{Gone: gone}
	for _, r := range refs {
		note.Refs = append(note.Refs, SourceRef{Path: r.Path, Line: r.Line, URL: r.URL})
	}
	return note
}

// diffSides are a unified diff's added and removed lines, each side's joined.
func diffSides(d string) (added, removed string) {
	var a, r strings.Builder
	for line := range strings.SplitSeq(d, "\n") {
		switch {
		case strings.HasPrefix(line, "@@"):
		case strings.HasPrefix(line, "+"):
			a.WriteString(line[1:] + "\n")
		case strings.HasPrefix(line, "-"):
			r.WriteString(line[1:] + "\n")
		}
	}
	return a.String(), r.String()
}

// needleOf is the name a change is about, as the source would spell it; "" for none.
func needleOf(c *Change) string {
	p := c.Path
	at := func(i int) string {
		if i < len(p) {
			return p[i]
		}
		return ""
	}
	switch c.Category {
	case "model":
		if at(1) == "default" || at(1) == "aliases" {
			if s, ok := c.After.(string); ok && c.Op != "removed" {
				return s
			}
			if s, ok := c.Before.(string); ok {
				return s
			}
		}
		if at(1) == "params" {
			return at(2)
		}
	case "tools":
		if at(2) == "input_schema" && at(3) == "properties" {
			return at(4)
		}
		name := at(1)
		if _, n, ok := strings.Cut(name, "/"); ok {
			name = n
		}
		return name
	case "telemetry":
		if at(3) == "keys" {
			return at(4)
		}
		if at(1) == "surfaces" {
			surface := at(2)
			if i := strings.LastIndex(surface, "/"); i >= 0 {
				surface = surface[i+1:]
			}
			return surface
		}
	case "request":
		switch at(1) {
		case "betas", "headers", "body_keys":
			return at(2)
		case "identifiers":
			return strings.TrimPrefix(at(2), "header:")
		}
	case "side_requests", "network":
		// The path a request goes to: "POST /analytics/codex/turn-costs".
		if i := strings.Index(at(1), "/"); i >= 0 {
			path := at(1)[i:]
			if j := strings.Index(path, " "); j >= 0 {
				path = path[:j]
			}
			return path
		}
	case "hooks":
		// A payload field by its last part ("tool_input.command" by "command"), an environment
		// variable by its name, anything else of an event's by the event.
		switch {
		case len(p) >= 5 && (at(3) == "keys" || at(3) == "kinds"):
			field := strings.TrimSuffix(at(4), "[]")
			if i := strings.LastIndex(field, "."); i >= 0 {
				field = strings.TrimSuffix(field[i+1:], "[]")
			}
			return field
		case len(p) >= 5 && (at(3) == "env" || at(3) == "env_paths"):
			return at(4)
		case at(1) == "source":
			return ""
		}
		return at(2)
	case "cli", "config":
		if at(1) != "help" {
			// A subcommand's flag is kept with its subcommand: "exec --json".
			fields := strings.Fields(at(2))
			if len(fields) > 0 {
				return fields[len(fields)-1]
			}
		}
	case "caching":
		return ""
	}
	return ""
}
