package e2e

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Normalizing what a trace saw: a path with the run's literals and the machine's paths named and
// its run's own values (ids, times, process ids, temporary names) placeholders, and a command
// line with the paths in it normalized the same way.

var (
	shortDigitsRE = regexp.MustCompile(`\d{2,}`)
	randomTokenRE = regexp.MustCompile(`[A-Za-z0-9]+`)
	// pathTokenRE is a path in a command line: from a slash, or a placeholder and a slash.
	pathTokenRE = regexp.MustCompile(`(?:<[a-z-]+>)?(?:/[^\s"'=:;|&()<>]+)+`)
)

// normalize is path with the run's literals in placeholders, and what varies by itself (ids,
// timestamps, process ids in a temporary file's name) in placeholders of its kind. A path inside
// a sandbox's new root as bubblewrap builds it (/newroot, /oldroot) is normalized below it.
func (s TraceScope) normalize(path string) string {
	for _, root := range []string{"/newroot", "/oldroot"} {
		if rest, ok := strings.CutPrefix(path, root+"/"); ok {
			if n := s.normalize("/" + rest); strings.HasPrefix(n, "<") {
				return root + "/" + n
			} else {
				return root + n
			}
		}
	}
	for _, lit := range s.literals() {
		if path == lit || strings.HasPrefix(path, lit+"/") {
			path = s.Replace[lit] + strings.TrimPrefix(path, lit)
			break
		}
	}
	return s.normalizeSegments(MachinePaths(path))
}

// literals are the scope's literals, longest first.
func (s TraceScope) literals() []string {
	return slices.DeleteFunc(slices.SortedFunc(maps.Keys(s.Replace), func(a, b string) int { return cmp.Compare(len(b), len(a)) }),
		func(lit string) bool { return lit == "" })
}

// normalizeSegments normalizes each segment of a path whose literals are replaced: a literal's
// slug (Claude Code names a project's folder by its path, every character but letters and
// digits a dash) as <name-slug>, and the values a run makes.
func (s TraceScope) normalizeSegments(path string) string {
	segs := strings.Split(path, "/")
	tmp := false
	for i, seg := range segs {
		if seg == "<tmp>" || seg == "tmp" || seg == ".tmp" || seg == "temp" {
			tmp = true
		}
		if strings.HasPrefix(seg, "<") && strings.HasSuffix(seg, ">") && strings.Count(seg, "<") == 1 {
			continue
		}
		for _, lit := range s.literals() {
			if len(lit) < 8 {
				continue
			}
			if slug := nonAlnumRE.ReplaceAllString(lit, "-"); strings.Contains(seg, slug) {
				seg = strings.ReplaceAll(seg, slug, strings.TrimSuffix(s.Replace[lit], ">")+"-slug>")
			}
		}
		// Git's loose objects sit in folders named by the first two digits of an id.
		if i > 0 && segs[i-1] == "objects" && i > 1 && segs[i-2] == ".git" && gitFanoutRE.MatchString(seg) {
			segs[i] = "<id>"
			continue
		}
		segs[i] = volatileSegment(seg, tmp)
	}
	return strings.Join(segs, "/")
}

var (
	nonAlnumRE  = regexp.MustCompile(`[^A-Za-z0-9]`)
	gitFanoutRE = regexp.MustCompile(`^[0-9a-f]{2}$`)
	// tempNameRE is a temporary file's random suffix: Rust's tempfile (.tmpXXXXXX), mktemp's
	// (tmp.XXXXXXXXXX), mkdtemp's after a dash (name-XXXXXX).
	tempNameRE = regexp.MustCompile(`(\.tmp|^tmp\.|\.tmp\.)([A-Za-z0-9]{6,10})$`)
	dashTempRE = regexp.MustCompile(`-([A-Za-z0-9]{6})$`)
	// hexTokenRE is a short random hex token, as a temporary name has one (claude-3f2a-cwd).
	hexTokenRE = regexp.MustCompile(`^[0-9a-f]{4,12}$`)
)

// volatileSegment is one path segment with its run's own values in placeholders. A segment
// with one (a temporary file's), or one in the temporary folder, has its other numbers and
// random tokens too: a process id beside a time.
func volatileSegment(seg string, inTmp bool) string {
	if seg == "" || seg == "." || seg == ".." {
		return seg
	}
	// A number alone, or a number's file (a process's: 56.json), is the run's.
	if digitStemRE.MatchString(seg) {
		return digitStemRE.ReplaceAllString(seg, "<n>$1")
	}
	was := seg
	seg = uuidRE.ReplaceAllString(seg, "<uuid>")
	seg = datetimeRE.ReplaceAllString(seg, "<time>")
	seg = hexRE.ReplaceAllString(seg, "<hash>")
	seg = tempNameRE.ReplaceAllString(seg, "$1<id>")
	// A file in the temporary folder named by a short random token alone, letters only now and
	// then (Claude Code's task output, tasks/bkjuwxtnl.output).
	if inTmp {
		seg = tmpTokenStemRE.ReplaceAllString(seg, "<id>$1")
	}
	if inTmp {
		seg = dashTempRE.ReplaceAllStringFunc(seg, func(m string) string {
			// mkdtemp's letters and digits, all of one case now and then (fswatch-probe-WJPDWS).
			if mixedToken(m[1:]) || strings.Trim(m[1:], "ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" {
				return "-<id>"
			}
			return m
		})
	}
	// A key's kind (id_ed25519) is no number.
	seg = strings.ReplaceAll(seg, "ed25519", "\x00")
	if seg == was && !inTmp && !strings.Contains(seg, ".tmp") && !strings.HasPrefix(seg, "tmp") && !digitsRE.MatchString(seg) {
		return strings.ReplaceAll(seg, "\x00", "ed25519")
	}
	// A token at a time: a number, letters and digits mixed, a short hex token in the
	// temporary folder.
	seg = randomTokenRE.ReplaceAllStringFunc(seg, func(tok string) string {
		switch {
		case inTmp && hexTokenRE.MatchString(tok):
			// In the temporary folder, a short hex token is random, digits only or not.
			return "<id>"
		case strings.Trim(tok, "0123456789") == "":
			if len(tok) >= 2 {
				return "<n>"
			}
		case len(tok) >= 4 && strings.ContainsAny(tok, "0123456789"), inTmp && len(tok) >= 6 && mixedToken(tok):
			return "<id>"
		}
		return tok
	})
	// A short token after one of the run's values is the run's too, whatever its letters
	// (Claude Code's shell snapshot, snapshot-zsh-<n>-rbqfzy.sh).
	seg = afterRunValueRE.ReplaceAllString(seg, "$1-<id>")
	return strings.ReplaceAll(seg, "\x00", "ed25519")
}

// installedDirs are where package managers install programs: a file the build reads there is
// dropped ("machine"), and a program it runs from there is named as MachinePaths names it.
var installedDirs = []string{"/opt/homebrew/", "/usr/local/Cellar/", "/usr/local/lib/node_modules/", "/usr/local/opt/", "/home/linuxbrew/"}

// mkdtempRE is a path ending in a dash and six letters or digits, as mkdtemp names a folder.
var mkdtempRE = regexp.MustCompile(`/[^/]*-[A-Za-z0-9]{6}$`)

// tmpTokenStemRE is a file name that is a short lower-case token and an extension.
var tmpTokenStemRE = regexp.MustCompile(`^[a-z0-9]{8,12}(\.[a-z]+)$`)

// afterRunValueRE is a placeholder, a dash and a six-character token.
var afterRunValueRE = regexp.MustCompile(`(<(?:n|id|hash|uuid|time)>)-[A-Za-z0-9]{6}\b`)

// digitStemRE is a segment that is a number, or a number and an extension.
var digitStemRE = regexp.MustCompile(`^\d+((?:\.[A-Za-z]+)*)$`)

// mixedToken reports a token a program made at random: letters of both cases, or letters and
// digits.
func mixedToken(tok string) bool {
	lower := strings.IndexFunc(tok, func(r rune) bool { return r >= 'a' && r <= 'z' }) >= 0
	upper := strings.IndexFunc(tok, func(r rune) bool { return r >= 'A' && r <= 'Z' }) >= 0
	digit := strings.ContainsAny(tok, "0123456789")
	return lower && upper || (lower || upper) && digit
}

// normalizeArgv is a command line with its paths and the run's values in placeholders, and
// the machine's paths named as MachinePaths names them: a program a package manager installed
// by its name, an Xcode's as <xcode>. The build's own helper processes take numbers
// (descriptors, process ids) the run picks: those are <n>.
func (s TraceScope) normalizeArgv(argv []string, exe string, own bool) []string {
	if len(argv) == 0 {
		argv = []string{exe}
	}
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = s.normalizeText(a)
		// A number the run picks: a helper's descriptors, a process id it asks about.
		if i > 0 && a != "" && strings.Trim(a, "0123456789") == "" && (own || len(a) >= 4) {
			out[i] = "<n>"
		}
	}
	return out
}

// normalizeText is a string with every path in it normalized: an argument may hold several
// (a shell's command).
func (s TraceScope) normalizeText(text string) string {
	if len(s.Path) > 1 {
		text = strings.ReplaceAll(text, s.Path, "<path>")
	}
	for _, lit := range s.literals() {
		text = strings.ReplaceAll(text, lit, s.Replace[lit])
	}
	for lit, name := range s.Endpoints {
		text = strings.ReplaceAll(text, lit, name)
	}
	// The machine's programs by their names, as a PATH lookup names them (/opt/homebrew/bin/git
	// is git), Xcode as <xcode> whichever version the runner image has: the same build's
	// commands read the same on any machine (machinepaths.go).
	text = MachinePaths(text)
	text = pathTokenRE.ReplaceAllStringFunc(text, s.normalizeSegments)
	// An id the run made, in a name (Claude Code's keychain entry is named by a hash of its
	// configuration folder).
	text = uuidRE.ReplaceAllString(text, "<uuid>")
	// A heredoc's delimiter the run picks at random (Claude Code's shell snapshot:
	// `<< 'PATH_END_c3ph6ooejs'`).
	text = heredocTagRE.ReplaceAllString(text, "${1}_<id>")
	return argHexRE.ReplaceAllStringFunc(text, func(tok string) string {
		if strings.ContainsAny(tok, "0123456789") {
			return "<hash>"
		}
		return tok
	})
}

// argHexRE is a hex token in a command line, eight digits or more.
var argHexRE = regexp.MustCompile(`\b[0-9a-f]{8,}\b`)

// heredocTagRE is an upper-case word with a random lower-case suffix: a heredoc delimiter a
// program made unique.
var heredocTagRE = regexp.MustCompile(`\b([A-Z][A-Z0-9]*(?:_[A-Z0-9]+)*)_[a-z0-9]{6,16}\b`)
