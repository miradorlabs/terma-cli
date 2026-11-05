package main

import (
	"errors"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// Lineage: since which version a harness has each of its subcommands, command-line options,
// configuration settings and environment variables, and until which, from every published
// snapshot of it (reference/SCHEMA.md, lineage.json). An item is keyed by its path in the
// snapshot, as a diff's change is (cli.commands.<command>.options.<flag>), so a reader joins
// the two. A build where the item's part was not observed (a section not captured, a command
// whose help was not read, a setting a minified reading missed) says nothing of it: it neither
// ends a range nor starts one. Where a harness's source is public, where an item already there
// in the oldest build tracked came from is looked up once (lineage_source.go) and kept.

// Lineage is docs/harnesses/<harness>/lineage.json. Nothing in it says when it was made: a
// history that did not change is a file that did not.
type Lineage struct {
	SchemaVersion int                     `json:"schema_version"`
	Harness       string                  `json:"harness"`
	Window        LineageWindow           `json:"window"`
	Items         map[string]*LineageItem `json:"items"`
}

// LineageWindow is the builds the lineage was read from.
type LineageWindow struct {
	Oldest string `json:"oldest"`
	Newest string `json:"newest"`
	Builds int    `json:"builds"`
}

// LineageItem is one item's history.
type LineageItem struct {
	Kind string `json:"kind"` // subcommand | option | setting | env_var
	// Ranges are the spans of builds that have it, oldest first: more than one where it went
	// and came back.
	Ranges []LineageRange `json:"ranges"`
	// SinceBounded is false where no build before the first range was seen without it (it is
	// in the oldest build tracked, or every build before it did not observe its part): the
	// first range's since is "or earlier". A source found makes it true.
	SinceBounded bool `json:"since_bounded"`
	// Source is the release whose source first defined it, and where, for an item a public
	// harness had before its oldest build tracked; nil where it was not looked up or could
	// not be told.
	Source *LineageSource `json:"source,omitempty"`
}

// LineageRange is a span of builds that have an item.
type LineageRange struct {
	Since string `json:"since"`
	// Until is the last build seen with it, nil while it is still there (the newest build that
	// observed its part has it).
	Until *string `json:"until"`
	// Removed is the first build seen without it, after Until; nil with Until.
	Removed *string `json:"removed"`
}

// LineageSource is where an item came from in its harness's public source: the earliest
// release tag that defines it and the commit that added it, and where it is defined there
// and at the newest build tracked.
type LineageSource struct {
	Tag     string `json:"tag"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	URL     string `json:"url"` // the commit
	SourceLocation
	// Verified says how the answer was checked: absent at the release before (named), present
	// at this one.
	Verified string `json:"verified"`
	// Current is where the newest build tracked defines it; nil where it is gone.
	Current *SourceLocation `json:"current,omitempty"`
}

// SourceLocation is a line of a release's source that defines an item.
type SourceLocation struct {
	Tag       string `json:"tag,omitempty"`
	Version   string `json:"version,omitempty"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	Permalink string `json:"permalink"`
}

// Lineage item kinds.
const (
	kindSubcommand = "subcommand"
	kindOption     = "option"
	kindSetting    = "setting"
	kindEnvVar     = "env_var"
)

// lineageFile is a harness's lineage, beside its index.
const lineageFile = "lineage.json"

// observation is what one build says of the items: those it has, and whether it observed
// the part an item it lacks would be in.
type observation struct {
	present map[string]string // id → kind
	known   func(id string) bool
}

// lineageOf is the history of every item the builds have, versions oldest first. Sources
// are not set.
func lineageOf(harness string, versions []string, byVersion map[string]*Snapshot) *Lineage {
	l := &Lineage{SchemaVersion: schemaVersion, Harness: harness, Items: map[string]*LineageItem{}}
	if len(versions) == 0 {
		return l
	}
	l.Window = LineageWindow{Oldest: versions[0], Newest: versions[len(versions)-1], Builds: len(versions)}
	ref := referenceKinds(versions, byVersion)
	obs := make([]observation, len(versions))
	kinds := map[string]string{}
	for i, v := range versions {
		obs[i] = observe(byVersion[v], ref)
		maps.Copy(kinds, obs[i].present)
	}
	for _, id := range slices.Sorted(maps.Keys(kinds)) {
		item := &LineageItem{Kind: kinds[id], Ranges: []LineageRange{}}
		absentSeen, last := false, ""
		open := false
		for i, v := range versions {
			if _, ok := obs[i].present[id]; ok {
				if !open {
					if len(item.Ranges) == 0 {
						item.SinceBounded = absentSeen
					}
					item.Ranges = append(item.Ranges, LineageRange{Since: v})
					open = true
				}
				last = v
				continue
			}
			if !obs[i].known(id) {
				continue
			}
			absentSeen = true
			if open {
				r := &item.Ranges[len(item.Ranges)-1]
				until, removed := last, v
				r.Until, r.Removed = &until, &removed
				open = false
			}
		}
		l.Items[id] = item
	}
	return l
}

// referenceKinds are how the newest build that has them read its configuration's options
// and variables: a build that read them another way is not compared with it, as a diff does
// not compare two such builds, so says nothing of them.
type refKinds struct{ options, env string }

func referenceKinds(versions []string, byVersion map[string]*Snapshot) refKinds {
	var r refKinds
	for i := len(versions) - 1; i >= 0; i-- {
		cfg := byVersion[versions[i]].Config
		if cfg == nil {
			continue
		}
		if _, ok := cfg["options"].(map[string]any); ok && r.options == "" {
			r.options = optionSourceKind(cfg)
		}
		if cfg["env_vars"] != nil && r.env == "" {
			r.env = envSourceKind(cfg)
		}
	}
	return r
}

func optionSourceKind(cfg map[string]any) string {
	s, _ := cfg["source"].(string)
	if i := strings.LastIndex(s, " at "); i >= 0 && !strings.Contains(s[i+4:], " ") {
		s = s[:i]
	}
	return s
}

func envSourceKind(cfg map[string]any) string {
	if s, _ := cfg["env_source"].(string); s != "" {
		return s
	}
	return "the names its strings hold"
}

// observe is what a snapshot says of the items.
func observe(s *Snapshot, ref refKinds) observation {
	o := observation{present: map[string]string{}}
	cmds, _ := s.CLI["commands"].(map[string]any)
	cliSeen := s.CLI != nil && cmds != nil
	helped := func(cmd string) bool {
		c, _ := cmds[cmd].(map[string]any)
		h, _ := c["helped"].(bool)
		return h
	}
	for name, raw := range cmds {
		c, _ := raw.(map[string]any)
		if strings.Contains(name, " ") {
			o.present["cli.commands."+name] = kindSubcommand
		}
		if h, _ := c["helped"].(bool); !h {
			continue // its options were not read
		}
		opts, _ := c["options"].(map[string]any)
		for flag := range opts {
			o.present["cli.commands."+name+".options."+flag] = kindOption
		}
	}
	// A command is known absent where the command that would list it was read and does not;
	// a command's options where its help was read, or it is known absent.
	var commandKnown func(cmd string) bool
	commandKnown = func(cmd string) bool {
		if !cliSeen {
			return false
		}
		if _, ok := cmds[cmd]; ok {
			return true
		}
		parent, _, ok := cutLast(cmd, " ")
		if !ok {
			return false // a root command not there: another binary's name
		}
		if _, ok := cmds[parent]; ok {
			return helped(parent)
		}
		return commandKnown(parent)
	}

	cfg := s.Config
	opts, _ := cfg["options"].(map[string]any)
	optsSeen := cfg != nil && opts != nil && optionSourceKind(cfg) == ref.options
	approx := strings.Contains(optionSourceKind(cfg), "minified")
	if optsSeen {
		for k := range opts {
			o.present["config.options."+k] = kindSetting
		}
	}
	envSeen := cfg != nil && cfg["env_vars"] != nil && envSourceKind(cfg) == ref.env
	if envSeen {
		for _, v := range anyStrings(cfg["env_vars"]) {
			o.present["config.env_vars."+v] = kindEnvVar
		}
	}
	unobservedTops := map[string]bool{}
	for _, v := range anyStrings(cfg["unobserved"]) {
		unobservedTops[v] = true
	}

	o.known = func(id string) bool {
		switch {
		case strings.HasPrefix(id, "cli.commands."):
			rest := strings.TrimPrefix(id, "cli.commands.")
			if cmd, _, ok := strings.Cut(rest, ".options."); ok {
				if _, there := cmds[cmd]; there {
					return helped(cmd)
				}
				return commandKnown(cmd)
			}
			return commandKnown(rest)
		case strings.HasPrefix(id, "config.options."):
			key := strings.TrimPrefix(id, "config.options.")
			if !optsSeen {
				return false
			}
			top, _, _ := strings.Cut(key, ".")
			top, _, _ = strings.Cut(top, "[")
			// As a diff reads a minified schema: a nested key it lacks, a top-level one builds
			// either side have, a table whose keys it could not follow, are not observed.
			if (approx && strings.ContainsAny(key, ".[")) || unobservedTops[top] || unreadTable(opts, key) {
				return false
			}
			return true
		case strings.HasPrefix(id, "config.env_vars."):
			return envSeen
		}
		return false
	}
	return o
}

// unreadTable reports whether opts has key's table but none of its keys: a table its reader
// could not follow, not one emptied.
func unreadTable(opts map[string]any, key string) bool {
	i := strings.LastIndex(key, ".")
	if i < 0 {
		return false
	}
	parent := strings.TrimSuffix(key[:i], "[]")
	if _, ok := opts[parent]; !ok {
		return false
	}
	for k := range opts {
		if strings.HasPrefix(k, parent+".") || strings.HasPrefix(k, parent+"[].") {
			return false
		}
	}
	return true
}

func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// anyStrings is a list of strings as JSON or Go holds it.
func anyStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// keepSources carries the sources an earlier lineage found over to l: they were looked up
// once, by hand, and a render never needs the source to keep them. A source applies to an item
// whose first range the builds leave open; one whose release is newer than the item's first
// build seen is kept but says no more than the builds do.
func keepSources(l, old *Lineage) {
	if old == nil {
		return
	}
	for id, item := range l.Items {
		if o := old.Items[id]; o != nil && o.Source != nil && item.Source == nil {
			item.Source = o.Source
		}
	}
	applySources(l)
}

// applySources makes each item's source its first range's start, where the builds leave it
// open.
func applySources(l *Lineage) {
	for _, item := range l.Items {
		if item.Source == nil || len(item.Ranges) == 0 || item.SinceBounded {
			continue
		}
		first := &item.Ranges[0]
		if !versionLess(first.Since, item.Source.Version) {
			first.Since = item.Source.Version
			item.SinceBounded = true
		}
	}
}

// readLineage is the lineage published in dir, nil where there is none.
func readLineage(dir string) (*Lineage, error) {
	var l Lineage
	if err := readJSON(filepath.Join(dir, lineageFile), &l); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &l, nil
}

// lineageSourcer finds where items came from in a harness's public source: nil where no
// source can be read.
type lineageSourcer interface {
	// lookup sets the source of each item of l that has none and that the source can tell.
	lookup(l *Lineage) error
	// refresh moves each source's current location to the newest build of l.
	refresh(l *Lineage) error
}

// writeLineage writes a harness's lineage into its folder, keeping the sources the one there
// found, and looking up or refreshing them where a sourcer can.
func writeLineage(dir, harness string, versions []string, byVersion map[string]*Snapshot, src lineageSourcer) error {
	l := lineageOf(harness, versions, byVersion)
	old, err := readLineage(dir)
	if err != nil {
		return err
	}
	keepSources(l, old)
	if src != nil {
		if err := src.lookup(l); err != nil {
			return err
		}
		if err := src.refresh(l); err != nil {
			return err
		}
		applySources(l)
	}
	return writeJSON(filepath.Join(dir, lineageFile), l)
}
