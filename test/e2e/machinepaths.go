package e2e

import (
	"regexp"
	"strings"
)

// Machine paths: what a path says of the machine that ran a build rather than of the build,
// which nothing published may hold. A desktop app the suite unpacked into its checkout, the
// runner's checkout of this repository, a version of Xcode, what a package manager installed
// and whose home it is are each the machine's: a runner image bump would change them, and a
// developer's machine would publish its user's name. MachinePaths is the one set of rules for
// them: the recorders apply it where they write what a build ran (TraceScope's command lines and
// paths, the desktop launch probe's variables), and refgen applies it again to every snapshot it
// publishes and every one it loads back, so a rule added here also fixes what is published.
//
// Each rule names the path as reference/SCHEMA.md does:
//
//	<app>/…                a desktop app's bundle, where the suite unpacked it
//	<checkout>/…           the runner's checkout (~/work/<repo>/<repo>)
//	<xcode>/…              an Xcode's developer folder, whichever Xcode_<version>.app it is
//	<xcode-app>/…          an Xcode's bundle
//	git, node              an installed program (a bin or sbin of Homebrew's, of /usr/local's
//	                       Cellar and opt, of npm's global modules, of Linuxbrew's): its name, as
//	                       a PATH lookup names it
//	<machine>/<name>       anything else such a package manager installed: its file's name
//	<home>/…               a user's home (/Users/<name>, the runner's /home/runner)
//
// The rules are for observed data alone (a path, a command line, an environment variable's
// value): a vendor's own text that names a path as an example (a system prompt's
// /home/user/monorepo, a tool's description's /Users/name/My Documents) is the build's, and is
// never passed through them.

// pathChars is one character a path segment in a command line or a value can hold.
const pathChars = `[^/\s"'=:;,|&()]`

var (
	// suiteAppRE is a desktop app's bundle under the suite's versions folder
	// (test/e2e/versions/<harness>/<version>/<Name>.app), from a checkout's path or a placeholder.
	suiteAppRE = regexp.MustCompile(`(?:<[a-z-]+>|(?:/` + pathChars + `+)+)/test/e2e/versions/` + pathChars + `+/` + pathChars + `+/` + pathChars + `+\.app\b`)
	// checkoutRE is a CI runner's checkout of a repository: ~/work/<repo>/<repo>, GitHub's.
	checkoutRE = regexp.MustCompile(`(^|[^\w./<>-])(?:/Users|/home)/` + pathChars + `+/work/[^/\s"'=:;,|&()_]` + pathChars + `*/[^/\s"'=:;,|&()_]` + pathChars + `*`)
	// xcodeDevRE and xcodeAppRE are an Xcode's developer folder and bundle, by any name a runner
	// image gives it (Xcode_26.6.app, Xcode-beta.app).
	xcodeDevRE = regexp.MustCompile(`/Applications/Xcode` + pathChars + `*\.app/Contents/Developer\b`)
	xcodeAppRE = regexp.MustCompile(`/Applications/Xcode` + pathChars + `*\.app\b`)
	// installedRE is a path under a package manager's prefix.
	installedRE = regexp.MustCompile(`(^|[^\w./<>-])(/home/linuxbrew/\.linuxbrew|/home/linuxbrew|/opt/homebrew|/usr/local/Cellar|/usr/local/opt|/usr/local/lib/node_modules)([^\s"'=:;,|&()<>]*)`)
	// homeRE is a user's home: any user's on macOS, the runner's on Linux (another /home/<name>
	// is as likely a build's own path: Claude Code's /home/claude/.claude/remote).
	homeRE = regexp.MustCompile(`(^|[^\w./<>-])(/(?:Users|home)/` + pathChars + `+)`)
)

// MachinePaths is s with the machine's paths in it named as reference/SCHEMA.md names them:
// longest first, a desktop app's bundle, then the checkout, Xcode, what a package manager
// installed and last a home, so each path is named by the most it says.
func MachinePaths(s string) string {
	if !strings.Contains(s, "/") {
		return s
	}
	s = suiteAppRE.ReplaceAllString(s, "<app>")
	s = checkoutRE.ReplaceAllString(s, "${1}<checkout>")
	s = xcodeDevRE.ReplaceAllString(s, "<xcode>")
	s = xcodeAppRE.ReplaceAllString(s, "<xcode-app>")
	s = installedRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := installedRE.FindStringSubmatch(m)
		if sub[3] != "" && sub[3][0] != '/' {
			return m // a longer name (/opt/homebrew-ish), not the prefix
		}
		return sub[1] + installedName(sub[3])
	})
	return homeRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := homeRE.FindStringSubmatch(m)
		if !isHome(sub[2]) {
			return m
		}
		return sub[1] + "<home>"
	})
}

// isHome reports whether dir, /Users/<name> or /home/<name>, is a home MachinePaths names: any
// user's on macOS but the machine's shared folder, and on Linux the runner's alone.
func isHome(dir string) bool {
	if name, ok := strings.CutPrefix(dir, "/Users/"); ok {
		return name != "Shared"
	}
	return dir == "/home/runner"
}

// installedName is what a path a package manager installed is published as, rest being the
// path below its prefix: a program in a bin or sbin its name, anything else <machine> and its
// file's name.
func installedName(rest string) string {
	segs := strings.Split(strings.Trim(rest, "/"), "/")
	switch {
	case rest == "" || rest == "/":
		return "<machine>"
	case len(segs) >= 2 && (segs[len(segs)-2] == "bin" || segs[len(segs)-2] == "sbin"):
		return segs[len(segs)-1]
	}
	return "<machine>/" + segs[len(segs)-1]
}

// machineLeaks are what no observed value published may hold once MachinePaths has named the
// machine's paths in it.
var machineLeaks = []string{"/Users/", "/opt/homebrew", "/Applications/Xcode", "/usr/local/Cellar", "/home/runner", "/home/linuxbrew"}

// MachineLeak is the first machine path s still holds ("/opt/homebrew"), "" for none: what
// refgen's publish-time guard looks for in every observed value.
func MachineLeak(s string) string {
	for _, l := range machineLeaks {
		for at := strings.Index(s, l); at >= 0; {
			if !strings.HasPrefix(s[at:], "/Users/Shared") {
				return l
			}
			next := strings.Index(s[at+1:], l)
			if next < 0 {
				break
			}
			at += 1 + next
		}
	}
	return ""
}
