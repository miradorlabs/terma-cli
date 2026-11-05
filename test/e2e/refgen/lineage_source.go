package main

import (
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/miradorlabs/terma-cli/e2e/sourcelink"
)

// Where a public harness's items came from: for each item already there in the oldest build
// tracked, the earliest release whose source defines it and the commit that added it, read
// from a local clone of its repository (lineage_clone.go). A one-time backfill, run by hand
// with -lineage-sources; every later render keeps what it found, and needs no clone. An item
// is read from a release the way a snapshot reads it: its command line by the commands and
// options clap's derive makes (lineage_clap.go), its settings by the configuration sources
// -source reads (config.go), its variables by the names its code reads (sourcelink). The
// releases are walked back from the oldest build tracked until one does not define the item;
// the answer is the release after it, and the commit, of those between the two releases that
// touch its definition, the first whose tree defines it and whose parent's does not. A release
// the reading cannot tell (no command line found, no settings read) or a commit not found
// leaves the item without a source rather than guess.
//
// Each source also says where its release defines the item, and where the newest build
// tracked does (current): a render moves current to a newer build by looking in the file it
// was in first, and searches the tree only where it is gone from there.

// lineageHarnesses are the harnesses whose lineage sources are looked up: the root struct of
// their command line, and the command it is.
var lineageHarnesses = map[string]struct{ root, bin string }{
	"codex": {root: "MultitoolCli", bin: "codex"},
}

// maxCandidates bounds the commits checked, of those a release's range has that touch an
// item's definition.
const maxCandidates = 8

// sourceSourcer looks up and refreshes a harness's lineage sources.
type sourceSourcer struct {
	harness string
	src     sourcelink.Source
	clone   *sourceClone        // nil: no lookups, and a current location read from lib
	lib     *sourcelink.Library // the run's, for the newest build's tree
	lookups bool
	log     io.Writer

	revs    map[string]*revIndex // by version, or commit
	envLibs map[string]*sourcelink.Library
}

// lineageSourcers is the run's sourcer for each harness: one that can look sources up where
// asked (lookup) and a clone can be had, one that only moves current locations where only the
// run's source library (-source) can be read, none otherwise.
func lineageSourcers(root string, links *sourceLinker, lookup bool) (func(string) lineageSourcer, error) {
	byHarness := map[string]lineageSourcer{}
	for h := range lineageHarnesses {
		s := sourcelink.Sources[h]
		ss := &sourceSourcer{harness: h, src: s, log: os.Stderr, revs: map[string]*revIndex{}}
		if lookup {
			c, err := ensureClone(root, s.Repo)
			if err != nil {
				return nil, err
			}
			ss.clone, ss.lookups = c, true
		} else {
			ss.clone = openClone(root, s.Repo)
		}
		if links != nil {
			ss.lib = links.lib
		}
		if ss.clone == nil && ss.lib == nil {
			continue
		}
		byHarness[h] = ss
	}
	return func(h string) lineageSourcer {
		if s, ok := byHarness[h]; ok {
			return s
		}
		return nil
	}, nil
}

// revIndex is what one release (or commit) of the source defines, by item id, where.
type revIndex struct {
	items map[string]SourceLocation
	// known says, by kind, whether the reading could tell: where it could not, an item of that
	// kind absent is not observed.
	known map[string]bool
	read  map[string]bool // the kinds read
	// settings is how its settings were read: the configuration source, without the file a
	// struct was found in.
	settings string
}

func (r *revIndex) has(id string) (bool, bool) {
	_, ok := r.items[id]
	return ok, r.known[kindOfID(id)]
}

// hasLike is has, where the reference read its settings as ref says: a release whose settings
// were read another way (a struct's fields before the schema was generated) tells only of the
// top-level ones, which every way reads alike; of a nested one it says nothing.
func (r *revIndex) hasLike(id, ref string) (bool, bool) {
	ok, known := r.has(id)
	if kindOfID(id) == kindSetting && r.settings != ref && strings.ContainsAny(strings.TrimPrefix(id, "config.options."), ".[") {
		return ok, false
	}
	return ok, known
}

// kindOfID is an item's kind from its id.
func kindOfID(id string) string {
	switch {
	case strings.HasPrefix(id, "cli.commands.") && strings.Contains(id, ".options."):
		return kindOption
	case strings.HasPrefix(id, "cli.commands."):
		return kindSubcommand
	case strings.HasPrefix(id, "config.options."):
		return kindSetting
	case strings.HasPrefix(id, "config.env_vars."):
		return kindEnvVar
	}
	return ""
}

// treeReader reads one tree of the source: its files, and the variables its code reads.
type treeReader struct {
	files func(want func(string) bool) (map[string][]byte, error)
	env   func() ([]string, error)
	close func()
}

// reader reads version (a release, or a commit by its full name) from the clone where there is
// one, else from the run's library.
//
// From a clone, its files are read once, straight from git (the parts of the tree that ship and
// the configuration files, of the kinds the lineage reads), and its variables through a
// tarball of the whole tree, as -source reads them.
func (s *sourceSourcer) reader(version string) treeReader {
	if s.clone == nil {
		return treeReader{
			files: func(want func(string) bool) (map[string][]byte, error) { return s.lib.Files(s.harness, version, want) },
			env:   func() ([]string, error) { return s.lib.EnvReads(s.harness, version) },
			close: func() {},
		}
	}
	var lib *sourcelink.Library
	var all map[string][]byte
	var err error
	read := false
	rev := version
	if !shaRE.MatchString(version) {
		rev = "refs/tags/" + s.src.TagOf(version)
	}
	configFiles := map[string]bool{}
	specs := slices.Clone(s.src.Dirs)
	for _, cs := range configSources[s.harness] {
		if cs.file != "" {
			configFiles[cs.file] = true
			if !s.ships(cs.file) {
				specs = append(specs, cs.file)
			}
		}
	}
	return treeReader{
		files: func(want func(string) bool) (map[string][]byte, error) {
			if !read {
				read = true
				all, err = s.clone.files(rev, specs, func(p string) bool {
					return configFiles[p] || strings.HasSuffix(p, ".rs") || strings.HasSuffix(p, ".json") || path.Base(p) == "Cargo.toml"
				})
			}
			if err != nil {
				return nil, err
			}
			out := map[string][]byte{}
			for p, data := range all {
				if want(p) {
					out[p] = data
				}
			}
			return out, nil
		},
		env: func() ([]string, error) {
			if lib == nil {
				lib = sourcelink.New("")
				lib.Open = cloneOpener(map[string]*sourceClone{s.src.Repo: s.clone})
			}
			return lib.EnvReads(s.harness, version)
		},
		close: func() {
			if lib != nil {
				lib.Close()
			}
		},
	}
}

// index is what a version of the source defines of the kinds asked for, each kind read once a
// run.
func (s *sourceSourcer) index(version string, kinds ...string) *revIndex {
	r := s.revs[version]
	if r == nil {
		r = &revIndex{items: map[string]SourceLocation{}, known: map[string]bool{}, read: map[string]bool{}}
		s.revs[version] = r
	}
	var need []string
	for _, k := range kinds {
		if k == kindSubcommand {
			k = kindOption // read together
		}
		if !r.read[k] && !slices.Contains(need, k) {
			need = append(need, k)
		}
	}
	if len(need) == 0 {
		return r
	}
	tr := s.reader(version)
	defer tr.close()
	at := func(file string, line int) SourceLocation {
		loc := SourceLocation{File: file, Line: line}
		if !shaRE.MatchString(version) {
			loc.Tag, loc.Version, loc.Permalink = s.src.TagOf(version), version, s.src.Blob(version, file, line)
		}
		return loc
	}
	for _, kind := range need {
		r.read[kind] = true
		switch kind {
		case kindOption:
			spec, ok := lineageHarnesses[s.harness]
			if !ok {
				continue
			}
			files, err := tr.files(func(p string) bool {
				return s.ships(p) && (strings.HasSuffix(p, ".rs") && !rustTestFile(p) || path.Base(p) == "Cargo.toml")
			})
			if err != nil {
				continue
			}
			clapFiles := map[string][]byte{}
			for p, data := range files {
				if path.Base(p) == "Cargo.toml" || strings.Contains(string(data), "clap") {
					clapFiles[p] = data
				}
			}
			if idx := newRustTree(clapFiles).clapIndex(spec.root, spec.bin); idx != nil {
				r.known[kindOption], r.known[kindSubcommand] = true, true
				for id, loc := range idx {
					r.items[id] = at(loc.file, loc.line)
				}
			}
		case kindSetting:
			for _, cs := range configSources[s.harness] {
				w := cs.want
				if w == nil {
					file := cs.file
					w = func(p string) bool { return p == file }
				}
				files, err := tr.files(w)
				if err != nil || len(files) == 0 {
					continue
				}
				opts, from := cs.read(files)
				if len(opts) == 0 {
					continue
				}
				r.known[kindSetting] = true
				r.settings = cs.file + cs.what
				file := from
				if _, ok := files[file]; !ok {
					if i := strings.LastIndex(from, " in "); i >= 0 { // "ConfigToml in <file>"
						file = from[i+4:]
					}
				}
				for key := range opts {
					r.items["config.options."+key] = at(file, settingLine(files[file], key))
				}
				break
			}
		case kindEnvVar:
			if names, err := tr.env(); err == nil {
				r.known[kindEnvVar] = true
				for _, n := range names {
					r.items["config.env_vars."+n] = SourceLocation{} // located on demand: envLocation
				}
			}
		}
	}
	return r
}

// ships reports whether a file is in the part of the tree that ships (the source's Dirs).
func (s *sourceSourcer) ships(p string) bool {
	if len(s.src.Dirs) == 0 {
		return true
	}
	return slices.ContainsFunc(s.src.Dirs, func(d string) bool { return strings.HasPrefix(p, d) })
}

// rustTestFile reports a file of tests, which defines no command the build has.
func rustTestFile(p string) bool {
	return strings.Contains(p, "/tests/") || strings.HasSuffix(p, "_tests.rs") || strings.HasSuffix(p, "_test.rs") || strings.Contains(p, "/benches/")
}

// settingLeaf is the last name of a setting's key: config_file of agents.<name>.config_file,
// mcp_servers of mcp_servers.<name>.
func settingLeaf(key string) string {
	parts := strings.Split(key, ".")
	for i := len(parts) - 1; i >= 0; i-- {
		p := strings.TrimSuffix(parts[i], "[]")
		if p != "" && !strings.HasPrefix(p, "<") {
			return p
		}
	}
	return key
}

// settingNeedle matches a line that defines a setting: its property in a JSON schema, its
// field in a struct.
func settingNeedle(key string) string {
	leaf := regexp.QuoteMeta(settingLeaf(key))
	return `"` + leaf + `"` + reSpaces + `:|` + wordStart + leaf + reSpaces + `:`
}

// settingLine is the line of data that defines key: of the lines that name its last part,
// the first after one that names its parent, or the first.
func settingLine(data []byte, key string) int {
	if data == nil {
		return 0
	}
	re := regexp.MustCompile(settingNeedle(key))
	parts := strings.Split(key, ".")
	parent := ""
	for i := len(parts) - 2; i >= 0; i-- {
		if p := strings.TrimSuffix(parts[i], "[]"); !strings.HasPrefix(p, "<") {
			parent = p
			break
		}
	}
	first, afterParent, seenParent := 0, 0, false
	for i, line := range strings.Split(string(data), "\n") {
		if parent != "" && strings.Contains(line, `"`+parent+`"`) {
			seenParent = true
		}
		if re.MatchString(line) {
			if first == 0 {
				first = i + 1
			}
			if seenParent && afterParent == 0 {
				afterParent = i + 1
			}
		}
	}
	if afterParent != 0 {
		return afterParent
	}
	return first
}

// Needles are POSIX extended regular expressions, which git log -G and Go's regexp both read:
// neither \b nor \s.
const (
	wordStart = `(^|[^A-Za-z0-9_])`
	wordEnd   = `([^A-Za-z0-9_]|$)`
	reSpaces  = `[[:space:]]*`
)

// needleOfID matches a line that defines an item, wherever it is: a flag's long name quoted or
// its field, a command's name quoted or its variant, a setting's property or field, a
// variable's name quoted.
func needleOfID(id string) string {
	switch kindOfID(id) {
	case kindOption:
		_, flag, _ := strings.Cut(id, ".options.")
		name := strings.TrimLeft(flag, "-")
		return `"` + regexp.QuoteMeta(name) + `"|` + wordStart + regexp.QuoteMeta(strings.ReplaceAll(name, "-", "_")) + reSpaces + `:`
	case kindSubcommand:
		cmd := strings.TrimPrefix(id, "cli.commands.")
		name := cmd[strings.LastIndex(cmd, " ")+1:]
		return `"` + regexp.QuoteMeta(name) + `"|` + wordStart + regexp.QuoteMeta(camel(name)) + wordEnd
	case kindSetting:
		return settingNeedle(strings.TrimPrefix(id, "config.options."))
	case kindEnvVar:
		return `"` + regexp.QuoteMeta(strings.TrimPrefix(id, "config.env_vars.")) + `"`
	}
	return ""
}

// camel is a command's name as its variant spells it: app-server is AppServer.
func camel(s string) string {
	var sb strings.Builder
	for part := range strings.SplitSeq(s, "-") {
		if part != "" {
			sb.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return sb.String()
}

// envLocation is where version's code reads a variable: the first line that quotes it.
// A version's tree is kept, and its names indexed, until closeEnvLibs: many variables are
// looked for in one version.
func (s *sourceSourcer) envLocation(version, name string) (SourceLocation, bool) {
	lib := s.lib
	if s.clone != nil {
		if s.envLibs == nil {
			s.envLibs = map[string]*sourcelink.Library{}
		}
		if lib = s.envLibs[version]; lib == nil {
			lib = sourcelink.New("")
			lib.Open = cloneOpener(map[string]*sourceClone{s.src.Repo: s.clone})
			s.envLibs[version] = lib
		}
	}
	found, err := lib.Find(s.harness, version, []string{name})
	if err != nil || len(found[name]) == 0 {
		return SourceLocation{}, false
	}
	r := found[name][0]
	return SourceLocation{Tag: s.src.TagOf(version), Version: version, File: r.Path, Line: r.Line, Permalink: r.URL}, true
}

// closeEnvLibs removes the trees envLocation kept, but those of keep.
func (s *sourceSourcer) closeEnvLibs(keep ...string) {
	for v, lib := range s.envLibs {
		if !slices.Contains(keep, v) {
			lib.Close()
			delete(s.envLibs, v)
		}
	}
}

// location is where version defines id, false where it does not.
func (s *sourceSourcer) location(version, id string) (SourceLocation, bool) {
	kind := kindOfID(id)
	if kind == kindEnvVar {
		return s.envLocation(version, id[len("config.env_vars."):])
	}
	r := s.index(version, kind)
	loc, ok := r.items[id]
	return loc, ok
}

func (s *sourceSourcer) logf(format string, args ...any) {
	if s.log != nil {
		fmt.Fprintf(s.log, "refgen: lineage %s: "+format+"\n", append([]any{s.harness}, args...)...)
	}
}

// lookup finds the source of each item in the oldest build tracked that has none.
func (s *sourceSourcer) lookup(l *Lineage) error {
	if !s.lookups || s.clone == nil || len(l.Items) == 0 {
		return nil
	}
	var open []string
	for _, id := range slices.Sorted(keysOf(l.Items)) {
		it := l.Items[id]
		if it.Source == nil && !it.SinceBounded && len(it.Ranges) > 0 && it.Ranges[0].Since == l.Window.Oldest {
			open = append(open, id)
		}
	}
	if len(open) == 0 {
		return nil
	}
	releases, err := s.clone.releases(s.src)
	if err != nil {
		return err
	}
	oldest := slices.Index(releases, l.Window.Oldest)
	if oldest < 0 {
		s.logf("no release tag %s in the clone", s.src.TagOf(l.Window.Oldest))
		return nil
	}
	kinds := []string{kindOption, kindSubcommand, kindSetting, kindEnvVar}
	s.logf("looking up %d items, from %s back", len(open), l.Window.Oldest)
	top := s.index(releases[oldest], kinds...)
	// since[id] is the oldest release found defining it so far, in a run back from the oldest
	// build; an id the oldest release does not define (as this reads it) cannot be told.
	since := map[string]int{}
	var walking []string
	for _, id := range open {
		if ok, known := top.has(id); ok && known {
			since[id] = oldest
			walking = append(walking, id)
		} else {
			s.logf("%s: not read from %s", id, s.src.TagOf(l.Window.Oldest))
		}
	}
	found := map[string]int{} // id → the release that introduced it
	firstRelease := map[string]bool{}
	// unread[id] is a release, older than since[id], the reading could not tell: the item
	// absent before it may have come in it, so is not told.
	unread := map[string]bool{}
	for i := oldest - 1; i >= 0 && len(walking) > 0; i-- {
		// Only the kinds still being walked: a release's variables take a tarball to read.
		var need []string
		for _, id := range walking {
			if k := kindOfID(id); !slices.Contains(need, k) {
				need = append(need, k)
			}
		}
		r := s.index(releases[i], need...)
		for _, k := range need {
			if !r.known[k] {
				s.logf("%s: its %ss could not be read", s.src.TagOf(releases[i]), k)
			}
		}
		next := walking[:0]
		for _, id := range walking {
			ok, known := r.hasLike(id, top.settings)
			switch {
			case !known:
				unread[id] = true
				next = append(next, id)
			case ok:
				since[id], unread[id] = i, false
				next = append(next, id)
			case unread[id]:
				s.logf("%s: a release between %s and %s could not be read: no source", id, s.src.TagOf(releases[i]), s.src.TagOf(releases[since[id]]))
			default:
				found[id] = since[id]
			}
		}
		walking = next
		s.logf("%s: %d items still defined", releases[i], len(walking))
	}
	for _, id := range walking {
		if !unread[id] {
			found[id], firstRelease[id] = since[id], true
		}
	}
	newest := l.Window.Newest
	n := 0
	ids := slices.Sorted(keysOf(found))
	// By release, so each release's tree is read once.
	slices.SortStableFunc(ids, func(a, b string) int { return found[a] - found[b] })
	defer s.closeEnvLibs()
	for _, id := range ids {
		i := found[id]
		s.closeEnvLibs(releases[i], newest)
		version := releases[i]
		prev := ""
		if i > 0 {
			prev = releases[i-1]
		}
		commit, ok := s.introducingCommit(id, prev, version)
		if !ok {
			s.logf("%s: no commit between %s and %s found that adds it: no source", id, prev, version)
			continue
		}
		loc, ok := s.location(version, id)
		if !ok {
			continue
		}
		verified := fmt.Sprintf("absent at %s, present at %s; %s adds it", s.src.TagOf(prev), s.src.TagOf(version), commit[:12])
		if firstRelease[id] {
			verified = fmt.Sprintf("present at %s, the first release; %s adds it", s.src.TagOf(version), commit[:12])
		}
		src := &LineageSource{Tag: s.src.TagOf(version), Version: version, Commit: commit,
			URL: s.src.URL() + "/commit/" + commit, SourceLocation: loc, Verified: verified}
		if cur, ok := s.location(newest, id); ok && stillThere(l.Items[id]) {
			src.Current = &cur
		}
		l.Items[id].Source = src
		n++
	}
	s.logf("%d of %d items sourced", n, len(open))
	return nil
}

// stillThere reports whether the newest build that observed an item has it.
func stillThere(it *LineageItem) bool {
	return len(it.Ranges) > 0 && it.Ranges[len(it.Ranges)-1].Until == nil
}

func keysOf[V any](m map[string]V) func(func(string) bool) { return keys(m) }

// introducingCommit is the commit after prev, up to version's tag, that added id: of those
// that touch a line its needle matches, the first whose tree defines it and whose first
// parent's does not.
func (s *sourceSourcer) introducingCommit(id, prev, version string) (string, bool) {
	kind := kindOfID(id)
	tag := "refs/tags/" + s.src.TagOf(version)
	rng := tag
	if prev != "" {
		rng = "refs/tags/" + s.src.TagOf(prev) + ".." + tag
	}
	needle := needleOfID(id)
	if kind == kindEnvVar {
		needle = ""
	}
	ref := s.index(version, kind).settings
	var paths []string
	if kind != kindEnvVar {
		if loc, ok := s.location(version, id); ok && loc.File != "" {
			paths = append(paths, loc.File)
		}
	}
	var dirs []string
	dirs = append(dirs, s.src.Dirs...)
	try := func(scope []string) []string {
		args := []string{"log", "--format=%H", "--reverse", "--topo-order"}
		if needle != "" {
			args = append(args, "-G"+needle)
		} else {
			args = append(args, "-S\""+id[len("config.env_vars."):]+"\"")
		}
		args = append(args, rng, "--")
		args = append(args, scope...)
		out, err := s.clone.git(nil, args...)
		if err != nil {
			return nil
		}
		return strings.Fields(string(out))
	}
	candidates := try(paths)
	if len(paths) == 0 || len(candidates) == 0 {
		candidates = try(dirs)
	}
	checked := 0
	for _, c := range candidates {
		if checked >= maxCandidates {
			break
		}
		checked++
		if ok, known := s.definesAt(c, id, kind, ref); !ok || !known {
			continue
		}
		parent, err := s.clone.git(nil, "rev-parse", c+"^")
		if err != nil {
			return c, true // a root commit
		}
		// Its parent seen without it: a parent read another way says nothing.
		if ok, known := s.definesAt(strings.TrimSpace(string(parent)), id, kind, ref); !ok && known {
			return c, true
		}
	}
	// Nothing that touches its own definition added it (an option a new command inherits, or
	// flattens): the first commit on the release's first-parent line that defines it, found by
	// halving the line, its parent seen without it.
	out, err := s.clone.git(nil, "rev-list", "--first-parent", "--reverse", rng)
	if err != nil {
		return "", false
	}
	line := strings.Fields(string(out))
	i := sort.Search(len(line), func(i int) bool {
		ok, known := s.definesAt(line[i], id, kind, ref)
		return ok && known
	})
	if i == len(line) {
		return "", false
	}
	parent, err := s.clone.git(nil, "rev-parse", line[i]+"^")
	if err != nil {
		return line[i], true // a root commit
	}
	if ok, known := s.definesAt(strings.TrimSpace(string(parent)), id, kind, ref); !ok && known {
		return line[i], true
	}
	return "", false
}

// definesAt reports whether a commit's tree defines id, and whether it could tell, its
// settings compared with those read as ref.
func (s *sourceSourcer) definesAt(commit, id, kind, ref string) (bool, bool) {
	return s.index(commit, kind).hasLike(id, ref)
}

// refresh moves each source's current location to the newest build tracked: where it was
// found before, in the file it was in, the matching line nearest the one it was on; or else
// wherever the tree defines it.
func (s *sourceSourcer) refresh(l *Lineage) error {
	defer s.closeEnvLibs()
	newest := l.Window.Newest
	tag := s.src.TagOf(newest)
	var stale []string
	files := map[string]bool{}
	for _, id := range slices.Sorted(keysOf(l.Items)) {
		it := l.Items[id]
		switch {
		case it.Source == nil:
		case !stillThere(it):
			it.Source.Current = nil
		case it.Source.Current != nil && it.Source.Current.Tag == tag:
		default:
			stale = append(stale, id)
			if c := it.Source.Current; c != nil && c.File != "" {
				files[c.File] = true
			}
		}
	}
	if len(stale) == 0 {
		return nil
	}
	// The files the stale locations were in, read in one pass over the tree.
	var read map[string][]byte
	if len(files) > 0 {
		tr := s.reader(newest)
		read, _ = tr.files(func(p string) bool { return files[p] })
		tr.close()
	}
	for _, id := range stale {
		src := l.Items[id].Source
		if c := src.Current; c != nil {
			if line := lineIn(read[c.File], needleOfID(id), c.Line); line > 0 {
				src.Current = &SourceLocation{Tag: tag, Version: newest, File: c.File, Line: line, Permalink: s.src.Blob(newest, c.File, line)}
				continue
			}
		}
		src.Current = nil
		if loc, ok := s.location(newest, id); ok {
			src.Current = &loc
		}
	}
	return nil
}

// lineIn is the line of data that needle matches nearest near, 0 where none does.
func lineIn(data []byte, needle string, near int) int {
	re, err := regexp.Compile(needle)
	if data == nil || err != nil {
		return 0
	}
	best := 0
	for i, line := range strings.Split(string(data), "\n") {
		if re.MatchString(line) && (best == 0 || abs(i+1-near) < abs(best-near)) {
			best = i + 1
		}
	}
	return best
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
