package main

import (
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Reading a Rust command line from its source: the commands and options clap's derive makes of
// a tree's structs and enums, keyed as a snapshot keys them (cli.commands.<command>, and
// cli.commands.<command>.options.<--flag>), each with the line that defines it. A struct that
// derives Parser or Args gives its fields' options, those of the structs it flattens, and the
// subcommands of the enum its subcommand field holds; an enum that derives Subcommand gives a
// command for each variant. What a help text shows is what is kept: a hidden argument or
// command is not, --help is on every command, --version where the command says version, and an
// option marked global is on every command beneath its own. Types are found as rustc would:
// through the file's use declarations, then the crate (by its Cargo.toml), then anywhere.

// rustTree is a tree's Rust: its files' items by crate.
type rustTree struct {
	files  map[string]*rustFile
	byName map[string][]*rustItem
	crates map[string]string // crate name (as code names it) → its directory
}

var (
	cargoPackageRE = regexp.MustCompile(`(?m)^\[package\][^\[]*?^name\s*=\s*"([^"]+)"`)
	cargoLibRE     = regexp.MustCompile(`(?m)^\[lib\][^\[]*?^name\s*=\s*"([^"]+)"`)
)

// newRustTree reads files (path → source; Cargo.toml files for the crates) into a tree.
func newRustTree(files map[string][]byte) *rustTree {
	t := &rustTree{files: map[string]*rustFile{}, byName: map[string][]*rustItem{}, crates: map[string]string{}}
	for p, data := range files {
		if path.Base(p) != "Cargo.toml" {
			continue
		}
		name := ""
		if m := cargoLibRE.FindSubmatch(data); m != nil {
			name = string(m[1])
		} else if m := cargoPackageRE.FindSubmatch(data); m != nil {
			name = string(m[1])
		}
		if name != "" {
			t.crates[strings.ReplaceAll(name, "-", "_")] = path.Dir(p)
		}
	}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		if !strings.HasSuffix(p, ".rs") {
			continue
		}
		f := parseRustFile(p, t.crateOf(p), string(files[p]))
		t.files[p] = f
		for _, it := range f.items {
			t.byName[it.name] = append(t.byName[it.name], it)
		}
	}
	return t
}

// crateOf is the crate a file is in: the one whose directory is the nearest above it.
func (t *rustTree) crateOf(p string) string {
	best, bestDir := "", ""
	for name, dir := range t.crates {
		if strings.HasPrefix(p, dir+"/") && len(dir) > len(bestDir) {
			best, bestDir = name, dir
		}
	}
	return best
}

// resolve finds the item a type names, from a file.
func (t *rustTree) resolve(from *rustFile, typ []rustTok) *rustItem {
	it := t.resolvePath(from, typePath(typ), 0)
	for hops := 0; it != nil && it.alias != nil && hops < 4; hops++ {
		it = t.resolvePath(t.files[it.file], typePath(it.alias), 0)
	}
	return it
}

// platformAlias is, of a file's aliases named name, the one a macOS build takes (the reference
// reads its command line on macOS), or else one no cfg limits.
func platformAlias(f *rustFile, name string) *rustItem {
	var plain, any *rustItem
	for _, it := range f.items {
		if it.name != name || it.alias == nil {
			continue
		}
		if any == nil {
			any = it
		}
		cfg := ""
		for _, a := range it.attrs {
			if a.name == "cfg" {
				cfg = joinAttr(a)
			}
		}
		switch {
		case strings.Contains(cfg, `target_os="macos"`) && !strings.HasPrefix(cfg, "not"):
			return it
		case cfg == "" && plain == nil:
			plain = it
		}
	}
	if plain != nil {
		return plain
	}
	return any
}

// joinAttr is an attribute's items as one text: target_os="macos", or not(...) for a
// predicate's call.
func joinAttr(a rustAttr) string {
	var parts []string
	for _, it := range a.items {
		switch {
		case it.str:
			parts = append(parts, it.key+`="`+it.value+`"`)
		case it.value != "":
			parts = append(parts, it.key+"("+it.value+")")
		default:
			parts = append(parts, it.key)
		}
	}
	return strings.Join(parts, ",")
}

func (t *rustTree) resolvePath(from *rustFile, p []string, hops int) *rustItem {
	if len(p) == 0 || hops > 6 {
		return nil
	}
	if len(p) == 1 {
		if use, ok := from.uses[p[0]]; ok && !(len(use) == 1 && use[0] == p[0]) {
			return t.resolvePath(from, use, hops+1)
		}
		for _, it := range from.items {
			if it.name == p[0] {
				if it.alias != nil {
					return platformAlias(from, p[0])
				}
				return it
			}
		}
		return t.inCrate(from.crate, p[0], nil, hops)
	}
	name := p[len(p)-1]
	mods := p[:len(p)-1]
	switch mods[0] {
	case "crate", "self", "super":
		return t.inCrate(from.crate, name, mods[1:], hops)
	}
	if _, ok := t.crates[mods[0]]; ok {
		return t.inCrate(mods[0], name, mods[1:], hops)
	}
	if use, ok := from.uses[mods[0]]; ok {
		return t.resolvePath(from, append(slices.Clone(use), p[1:]...), hops+1)
	}
	return t.inCrate(from.crate, name, mods, hops)
}

// inCrate finds name in a crate: an item of that name (in the module mods names, where more
// than one has it), or a name its files re-export under it.
func (t *rustTree) inCrate(crate, name string, mods []string, hops int) *rustItem {
	var found []*rustItem
	for _, it := range t.byName[name] {
		if it.crate == crate {
			found = append(found, it)
		}
	}
	if len(found) > 1 && len(mods) > 0 {
		for _, it := range found {
			if strings.Contains(it.file, "/"+mods[len(mods)-1]) {
				return it
			}
		}
	}
	if len(found) > 0 {
		return found[0]
	}
	for _, p := range slices.Sorted(maps.Keys(t.files)) {
		f := t.files[p]
		if f.crate != crate {
			continue
		}
		if use, ok := f.uses[name]; ok && use[len(use)-1] != name || ok && len(use) > 1 && use[0] != "crate" {
			if it := t.resolvePath(f, use, hops+1); it != nil {
				return it
			}
		}
	}
	if all := t.byName[name]; len(all) == 1 {
		return all[0]
	}
	return nil
}

// typePath is the path a type names, its wrappers (Option, Box) taken off.
func typePath(typ []rustTok) []string {
	for {
		var p []string
		i := 0
		for i < len(typ) && (typ[i].kind == 'i' || typ[i].text == "::" || typ[i].text == "&") {
			if typ[i].kind == 'i' && typ[i].text != "mut" && typ[i].text != "dyn" {
				p = append(p, typ[i].text)
			}
			i++
		}
		if len(p) == 0 {
			return nil
		}
		last := p[len(p)-1]
		if (last == "Option" || last == "Box") && i < len(typ) && typ[i].text == "<" {
			typ = typ[i+1:]
			if n := len(typ); n > 0 && typ[n-1].text == ">" {
				typ = typ[:n-1]
			}
			continue
		}
		return p
	}
}

// clapLocation is where a command or an option is defined.
type clapLocation struct {
	file string
	line int
}

// clapOption is an option a struct gives, with whether it is global.
type clapOption struct {
	key    string
	global bool
	loc    clapLocation
}

// clapIndex is every command and option of the command line the tree's root struct is, by
// id; nil where the tree has no such root.
func (t *rustTree) clapIndex(root, bin string) map[string]clapLocation {
	var it *rustItem
	for _, cand := range t.byName[root] {
		if cand.derives("Parser") {
			it = cand
			break
		}
	}
	if it == nil {
		return nil
	}
	out := map[string]clapLocation{}
	t.command(out, bin, it, nil, nil, 0)
	return out
}

// command adds a command's options, with those inherited, and its subcommands.
func (t *rustTree) command(out map[string]clapLocation, name string, it *rustItem, inline []rustField, inherited []clapOption, depth int) {
	if depth > 8 {
		return
	}
	var opts []clapOption
	var subs []*rustItem
	version := false
	var fields []rustField
	var from *rustFile
	if it != nil {
		fields = it.fields
		from = t.files[it.file]
		version = commandSays(it.attrs, "version")
	} else {
		fields = inline
	}
	t.args(&opts, &subs, &version, fields, from, 0)
	help := clapOption{key: "--help"}
	if it != nil {
		help.loc = clapLocation{file: it.file, line: it.line}
	}
	if it == nil || !commandSays(it.attrs, "disable_help_flag") {
		opts = append(opts, help)
	}
	if version {
		opts = append(opts, clapOption{key: "--version", loc: help.loc})
	}
	all := append(slices.Clone(inherited), opts...)
	var globals []clapOption
	for _, o := range all {
		id := "cli.commands." + name + ".options." + o.key
		if _, seen := out[id]; !seen {
			out[id] = o.loc
		}
		if o.global {
			globals = append(globals, o)
		}
	}
	for _, sub := range subs {
		t.subcommands(out, name, sub, globals, depth)
	}
}

// commandSays reports whether a struct's command attributes set key (version, or a flag such
// as disable_help_flag = true).
func commandSays(attrs []rustAttr, key string) bool {
	for _, a := range attrs {
		if !a.clap() {
			continue
		}
		if v, ok := a.get(key); ok && (v == "" || v == "true" || key == "version") {
			return true
		}
	}
	return false
}

// args collects the options of fields, through the structs they flatten, and the enums their
// subcommand fields hold.
func (t *rustTree) args(opts *[]clapOption, subs *[]*rustItem, version *bool, fields []rustField, from *rustFile, depth int) {
	if depth > 6 {
		return
	}
	for _, f := range fields {
		var clapAttrs []rustAttr
		for _, a := range f.attrs {
			if a.clap() {
				clapAttrs = append(clapAttrs, a)
			}
		}
		has := func(k string) bool { return slices.ContainsFunc(clapAttrs, func(a rustAttr) bool { return a.has(k) }) }
		get := func(k string) (string, bool) {
			for _, a := range clapAttrs {
				if v, ok := a.get(k); ok {
					return v, true
				}
			}
			return "", false
		}
		if v, ok := get("hide"); ok && (v == "" || v == "true") {
			continue
		}
		switch {
		case has("flatten"):
			if from == nil {
				continue
			}
			sub := t.resolve(from, f.typ)
			// A newtype that implements Args by hand around another (struct
			// TuiSharedCliOptions(SharedCliOptions)) flattens what it wraps.
			for hops := 0; sub != nil && !sub.enum && len(sub.fields) == 0 && len(sub.tuple) == 1 && hops < 3; hops++ {
				sub = t.resolve(t.files[sub.file], sub.tuple[0])
			}
			if sub != nil && !sub.enum {
				*version = *version || commandSays(sub.attrs, "version")
				t.args(opts, subs, version, sub.fields, t.files[sub.file], depth+1)
			}
		case has("subcommand"):
			if from == nil {
				continue
			}
			if sub := t.resolve(from, f.typ); sub != nil && sub.enum {
				*subs = append(*subs, sub)
			}
		case has("long") || has("short"):
			key, line := "", f.line
			if v, ok := get("long"); ok {
				if v == "" {
					v = strings.ReplaceAll(f.name, "_", "-")
					v = strings.TrimPrefix(v, "r#")
				} else {
					// Named in its attribute: that line defines it.
					for _, a := range clapAttrs {
						for _, it := range a.items {
							if it.key == "long" {
								line = it.line
							}
						}
					}
				}
				key = "--" + v
			} else {
				v, _ := get("short")
				if v == "" && f.name != "" {
					v = f.name[:1]
				}
				key = "-" + v
			}
			g, _ := get("global")
			p := ""
			if from != nil {
				p = from.path
			}
			*opts = append(*opts, clapOption{key: key, global: has("global") && (g == "" || g == "true"), loc: clapLocation{file: p, line: line}})
		}
	}
}

// subcommands adds each command an enum's variants make under parent.
func (t *rustTree) subcommands(out map[string]clapLocation, parent string, en *rustItem, globals []clapOption, depth int) {
	from := t.files[en.file]
	for _, v := range en.variants {
		var get func(string) (string, bool)
		has := func(k string) bool {
			return slices.ContainsFunc(v.attrs, func(a rustAttr) bool { return a.clap() && a.has(k) })
		}
		get = func(k string) (string, bool) {
			for _, a := range v.attrs {
				if a.clap() {
					if x, ok := a.get(k); ok {
						return x, true
					}
				}
			}
			return "", false
		}
		if x, ok := get("hide"); ok && (x == "" || x == "true") {
			continue
		}
		if has("external_subcommand") {
			continue
		}
		if has("flatten") && len(v.tuple) == 1 {
			if sub := t.resolve(from, v.tuple[0]); sub != nil && sub.enum {
				t.subcommands(out, parent, sub, globals, depth+1)
			}
			continue
		}
		name, explicit := get("name")
		if !explicit || name == "" {
			name = kebab(v.name)
		}
		cmd := parent + " " + name
		id := "cli.commands." + cmd
		if _, seen := out[id]; !seen {
			out[id] = clapLocation{file: en.file, line: v.line}
		}
		switch {
		case len(v.tuple) == 1:
			sub := t.resolve(from, v.tuple[0])
			switch {
			case sub == nil:
				t.command(out, cmd, nil, nil, globals, depth+1)
			case sub.enum && sub.derives("Subcommand"):
				t.command(out, cmd, nil, nil, globals, depth+1)
				t.subcommands(out, cmd, sub, globals, depth+1)
			default:
				t.command(out, cmd, sub, nil, globals, depth+1)
			}
		case len(v.fields) > 0:
			// A struct variant: its fields are the command's, in the enum's file.
			holder := &rustItem{file: en.file, crate: en.crate, name: v.name, fields: v.fields, attrs: v.attrs, line: v.line}
			t.command(out, cmd, holder, nil, globals, depth+1)
		default:
			t.command(out, cmd, nil, nil, globals, depth+1)
		}
	}
}

// kebab is a variant's name as clap names its command: AppServer is app-server.
func kebab(s string) string {
	var sb strings.Builder
	rs := []rune(s)
	for i, r := range rs {
		if unicode.IsUpper(r) {
			if i > 0 && (unicode.IsLower(rs[i-1]) || unicode.IsDigit(rs[i-1]) || i+1 < len(rs) && unicode.IsLower(rs[i+1]) && unicode.IsUpper(rs[i-1])) {
				sb.WriteByte('-')
			}
			sb.WriteRune(unicode.ToLower(r))
			continue
		}
		if r == '_' {
			sb.WriteByte('-')
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}
