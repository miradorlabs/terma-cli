package hookrun

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// What ShellPaths knows of the commands it walks: those that run another (wrappers), run a
// script an option gives (scripts) or are known not to read below a folder (shallow), and
// the options that take a folder or a value.

// wrapper is a command that runs the one its words name after its own: past its options,
// those in values taking the next word as theirs, NAME=value words and operands words
// (timeout's duration, flock's file). A chdir option's value is the folder it runs the
// command in (env -C), a split option's is split into the command's first words (env -S),
// and a shell option's is a shell script it runs (flock -c); a script wrapper runs its
// words as one (watch), or each as one of its own (hyperfine), and with a names option it
// runs nothing (command -v). One with a sub is a wrapper only when that word follows it (uv
// run).
type wrapper struct {
	sub                                string
	values, chdir, split, shell, names []string
	operands                           int
	script, each                       bool
}

// Commands by what they do with their words: wrappers run the command after their own;
// scripts run a script an option gives (scriptOf); folderOptions take a folder, as the next
// word or after their =, as -C does for dashC (for git only before its subcommand: git diff
// -C finds copies); valueOptions are a command's options before its subcommand that take
// the next word (git -c k=v log); shallow programs read no folder below one they name, but
// with an option or a subcommand that makes them (ls -R, git grep).
var (
	wrappers = map[string]wrapper{
		"timeout":    {values: []string{"-s", "--signal", "-k", "--kill-after"}, operands: 1},
		"env":        {values: []string{"-u", "--unset"}, chdir: []string{"-C", "--chdir"}, split: []string{"-S", "--split-string"}},
		"sudo":       {values: []string{"-u", "--user", "-g", "--group", "-h", "--host", "-p", "--prompt", "-r", "--role", "-t", "--type", "-C", "--close-from", "-T", "--command-timeout", "-U", "--other-user"}, chdir: []string{"-D", "--chdir"}},
		"doas":       {values: []string{"-u", "-C"}},
		"nice":       {values: []string{"-n", "--adjustment"}},
		"ionice":     {values: []string{"-c", "--class", "-n", "--classdata"}},
		"stdbuf":     {values: []string{"-i", "-o", "-e", "--input", "--output", "--error"}},
		"nohup":      {},
		"exec":       {values: []string{"-a"}},
		"time":       {values: []string{"-f", "--format", "-o", "--output"}},
		"command":    {names: []string{"-v", "-V"}},
		"builtin":    {},
		"caffeinate": {values: []string{"-t", "-w"}},
		"xargs":      {values: []string{"-I", "-n", "-L", "-P", "-s", "-E", "-d", "-a", "--replace", "--max-args", "--max-lines", "--max-procs", "--max-chars", "--eof", "--delimiter", "--arg-file"}},
		"watch":      {values: []string{"-n", "--interval"}, script: true},
		"hyperfine":  {script: true, each: true},
		"flock":      {values: []string{"-w", "--timeout", "-E", "--conflict-exit-code"}, shell: []string{"-c", "--command"}, operands: 1},
		"uv":         {sub: "run", values: []string{"--with", "--python", "-p", "--project", "--directory", "--package", "--extra", "--group", "--env-file", "--index"}},
		"poetry":     {sub: "run"},
		"pipenv":     {sub: "run"},
		"bundle":     {sub: "exec"},
		"pnpm":       {sub: "exec", values: []string{"--filter", "-F"}},
		"npm":        {sub: "exec", values: []string{"--package", "-w", "--workspace"}, shell: []string{"-c", "--call"}},
		"npx":        {values: []string{"-p", "--package"}, shell: []string{"-c", "--call"}},
	}
	shellScript = script{letters: "c", words: []string{"--command"}, shell: true}
	scripts     = map[string]script{
		"bash": shellScript, "sh": shellScript, "zsh": shellScript, "dash": shellScript, "ksh": shellScript, "ash": shellScript, "fish": shellScript, "su": shellScript,
		"python": {letters: "c"}, "pypy": {letters: "c"},
		"node": {letters: "ep", words: []string{"--eval", "--print"}}, "nodejs": {letters: "ep", words: []string{"--eval", "--print"}},
		"bun": {letters: "e", words: []string{"--eval", "--print"}}, "deno": {words: []string{"eval"}},
		"ruby": {letters: "e"}, "perl": {letters: "eE"}, "php": {letters: "r"},
	}
	folderOptions = []string{"--git-dir", "--work-tree", "--prefix", "--cwd", "--dir", "--directory"}
	dashC         = []string{"git", "make", "tar", "env"}
	valueOptions  = map[string][]string{"git": {"-c", "--config-env", "--namespace", "--super-prefix"}}
	grep          = reader{letters: "rR", words: []string{"--recursive", "--dereference-recursive", "--directories=recurse", "-d recurse"}}
	shallow       = map[string]reader{
		"ls": {letters: "R", words: []string{"--recursive"}}, "git": {sub: "grep"},
		"grep": grep, "egrep": grep, "fgrep": grep, "zgrep": grep,
		"cp":  {letters: "rRa", words: []string{"--recursive", "--archive"}},
		"tar": {letters: "c", words: []string{"--create"}, bare: true},
		"zip": {letters: "r", words: []string{"--recurse-paths"}},
		"cd":  {}, "pushd": {}, "popd": {}, "pwd": {}, "mkdir": {}, "rmdir": {}, "touch": {}, "mv": {}, "ln": {},
		"stat": {}, "file": {}, "test": {}, "[": {}, "echo": {}, "printf": {}, "realpath": {}, "readlink": {},
		"basename": {}, "dirname": {}, "which": {}, "command": {}, "true": {}, "false": {},
	}
	// depthOptions are the options whose value bounds how deep a program reads below a
	// folder, besides -maxdepth and --max-depth (find, rg, du).
	depthOptions = map[string]string{"fd": "-d", "tree": "-L"}
)

// script is how a program runs a script an option gives: the letters of its short options
// whose value it is, alone or ending a cluster (-c, -lc), its long ones or words (--eval,
// deno eval), and whether it is a shell's, read as commands, or an interpreter's, read for
// its slashed words.
type script struct {
	letters string
	words   []string
	shell   bool
}

// takes reports whether opt is an option whose value is s's script.
func (s script) takes(opt string) bool {
	return scriptFlag(opt, s.letters) || slices.Contains(s.words, opt)
}

// scriptOf is how the program name runs a script, matched past a version (python3.12,
// bash5), and whether it does.
func scriptOf(name string) (script, bool) {
	s, ok := scripts[strings.TrimRight(name, "0123456789.")]
	return s, ok
}

// reader is a shallow program's options that make it read below each folder it names: a
// short one's letter in any cluster of them (grep -rn), with no dash in its first word when
// bare (tar czf), or a word, an option's value included (-d recurse); or its subcommand sub
// (git grep).
type reader struct {
	letters, sub string
	words        []string
	bare         bool
}

// recursive reports whether opt, followed by next, the first of the command's words when
// first, makes r read below its folders.
func (r reader) recursive(opt, next string, first bool) bool {
	short := len(opt) > 1 && opt[0] == '-' && opt[1] != '-' || r.bare && first && opt != "" && opt[0] != '-'
	return short && r.letters != "" && strings.ContainsAny(strings.TrimPrefix(opt, "-"), r.letters) ||
		slices.Contains(r.words, opt) || slices.Contains(r.words, opt+" "+next)
}

// shellAt is the index among args, a command's words past its program, of a shell given its
// script with -c past its options (nix develop -c bash -c, strace -f /bin/zsh -lc), or -1.
func shellAt(args []*syntax.Word) int {
	for k, w := range args {
		s, ok := scriptOf(programName(w))
		if !ok || !s.shell {
			continue
		}
		for _, o := range args[k+1:] {
			if s.takes(o.Lit()) {
				return k
			}
			if !strings.HasPrefix(o.Lit(), "-") {
				break
			}
		}
	}
	return -1
}

// wrapped is a simple command's words past its wrappers: the wrappers' own (before) and the
// role each plays there (a folder option's value, a target for the folder one runs the
// command in, env -C's), a string one splits into the command's first words (split) or the
// shell script one runs instead (script, each word one when each), and the command, its
// program first.
type wrapped struct {
	before, script, command []*syntax.Word
	roles                   []role
	split                   *syntax.Word
	each                    bool
}

// unwrap is args, a simple command's words, past its wrappers, however they nest (sudo
// timeout 5 env A=1 bash -c ...).
func unwrap(args []*syntax.Word) wrapped {
	u := wrapped{command: args}
	for len(u.command) > 1 {
		w, ok := wrappers[programName(u.command[0])]
		n := 1
		if ok && w.sub != "" {
			ok, n = u.command[1].Lit() == w.sub, 2
		}
		if !ok {
			break
		}
		roles := slices.Repeat([]role{program}, n)
	options:
		for operands := 0; n < len(u.command); n++ {
			word, value := u.command[n], n+1 < len(u.command)
			opt, lead := word.Lit(), leadingLit(word)
			switch {
			case slices.Contains(w.names, opt):
				return u // command -v runs nothing
			case opt == "--":
				n++
				roles = append(roles, program)
				break options
			case value && slices.Contains(w.split, opt):
				u.before, u.roles = append(u.before, u.command[:n+1]...), append(append(u.roles, roles...), program)
				u.split, u.command = u.command[n+1], u.command[n+2:]
				return u
			case value && slices.Contains(w.shell, opt):
				u.before, u.roles = append(u.before, u.command[:n+1]...), append(append(u.roles, roles...), program)
				u.script, u.command = u.command[n+1:n+2], nil
				return u
			case value && slices.Contains(w.chdir, opt):
				n++
				roles = append(roles, program, target)
			case value && slices.Contains(w.values, opt):
				n++
				r := program
				if slices.Contains(folderOptions, opt) {
					r = folder // uv run --directory
				}
				roles = append(roles, program, r)
			case slices.ContainsFunc(w.chdir, func(o string) bool { return attached(word, o) }):
				roles = append(roles, folder) // env -C../x: a folder, not followed
			case strings.HasPrefix(lead, "-") && lead != "-", isAssignment(word):
				roles = append(roles, program)
			case operands < w.operands:
				operands++
				roles = append(roles, program)
			default:
				break options
			}
		}
		u.before, u.roles, u.command = append(u.before, u.command[:n]...), append(u.roles, roles...), u.command[n:]
		if w.script {
			u.script, u.command, u.each = u.command, nil, w.each
		}
	}
	return u
}

// leadingLit is the literal text w starts with.
func leadingLit(w *syntax.Word) string {
	if len(w.Parts) == 0 {
		return ""
	}
	if lead, ok := w.Parts[0].(*syntax.Lit); ok {
		return lead.Value
	}
	return ""
}

// attached reports whether w is the option opt with its value written against it (-Cdir,
// --chdir=dir).
func attached(w *syntax.Word, opt string) bool {
	lead := leadingLit(w)
	if strings.HasPrefix(opt, "--") {
		return strings.HasPrefix(lead, opt+"=")
	}
	return strings.HasPrefix(lead, opt) && w.Lit() != opt
}

// isAssignment reports whether w is NAME=value, as env takes one.
func isAssignment(w *syntax.Word) bool {
	name, _, eq := strings.Cut(leadingLit(w), "=")
	return eq && isName(name)
}

// programName is the base name of the program w names; "" when that is no plain word.
func programName(w *syntax.Word) string {
	if name := w.Lit(); name != "" {
		return strings.TrimSuffix(filepath.Base(name), ".exe")
	}
	return ""
}

// scriptFlag reports whether opt is a short option cluster ending in one of letters (-c,
// -lc, -ne), whose script is the next word.
func scriptFlag(opt, letters string) bool {
	return len(opt) >= 2 && len(opt) <= 4 && opt[0] == '-' && strings.ContainsRune(letters, rune(opt[len(opt)-1])) &&
		!strings.ContainsFunc(opt[1:], func(r rune) bool { return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') })
}

// folderValue is folder for a word naming a folder after its option (--dir=x, or -Cx where
// -C takes a folder), else plain.
func folderValue(w *syntax.Word, dashCFolder bool) role {
	if lead, ok := w.Parts[0].(*syntax.Lit); ok {
		opt, _, eq := strings.Cut(lead.Value, "=")
		if eq && slices.Contains(folderOptions, opt) || dashCFolder && strings.HasPrefix(opt, "-C") && w.Lit() != "-C" {
			return folder
		}
	}
	return plain
}

// run reads a command past its wrappers: its program, then its arguments, a cd's or
// pushd's target, a popd, an inline script (eval's, trap's, a shell's or an interpreter's,
// a shell's -c script wherever it is among the words: strace -f bash -c) and a folder
// option's value read as such. The words after -C (tar, make, git) are read from the folder
// it names. Unless the program is known not to read below a folder (shallow), it reads below
// each folder its words name, or where it runs when none names anything there (cd .. &&
// pytest), as deep as its depth option allows (find -maxdepth).
func (sc *scan) run(args []*syntax.Word) {
	sc.arg(args[0], program)
	name, args := programName(args[0]), args[1:]
	switch name {
	case "cd", "pushd":
		sc.cd(args, name == "pushd")
		return
	case "popd":
		if n := len(sc.pushed); n > 0 {
			sc.prev, sc.dir, sc.pushed = sc.dir, sc.pushed[n-1], sc.pushed[:n-1]
		}
		return
	case "eval":
		sc.inline(args, true)
		return
	case "trap":
		sc.inline(args[:min(len(args), 1)], true)
		return
	}
	if k := shellAt(args); k >= 0 {
		for _, w := range args[:k] {
			sc.arg(w, plain)
		}
		sc.run(args[k:])
		return
	}
	s, scripted := scriptOf(name)
	r, isShallow := shallow[name]
	deep, sub, levels := !isShallow, "", maxBelowDepth+2
	outer, dir, prev, moved := sc.reads, sc.dir, sc.prev, false
	sc.reads = &operands{}
	defer func() {
		if sc.reads = outer; moved {
			sc.dir, sc.prev = dir, prev // -C's folder is the command's alone
		}
	}()
	for j := 0; j < len(args); j++ {
		opt, next, after := args[j].Lit(), j+1 < len(args), ""
		if next {
			after = args[j+1].Lit()
		}
		dashCFolder := slices.Contains(dashC, name) && (name != "git" || sub == "")
		switch {
		case next && scripted && s.takes(opt):
			j++
			sc.inline(args[j:j+1], s.shell)
		case next && (slices.Contains(folderOptions, opt) || opt == "-C" && dashCFolder):
			j++
			sc.arg(args[j], folder)
			if opt == "-C" && name != "env" {
				moved = sc.into(args[j]) || moved
			}
		case next && sub == "" && slices.Contains(valueOptions[name], opt):
			j++
			sc.arg(args[j], plain)
		case next && depthOption(name, opt):
			j++
			if n, err := strconv.Atoi(after); err == nil {
				levels = n
			}
		default:
			deep = deep || isShallow && r.recursive(opt, after, j == 0)
			if v, ok := strings.CutPrefix(opt, "--max-depth="); ok {
				if n, err := strconv.Atoi(v); err == nil {
					levels = n
				}
			}
			if sub == "" && !strings.HasPrefix(opt, "-") {
				sub = opt
			}
			sc.arg(args[j], folderValue(args[j], dashCFolder))
		}
	}
	deep = deep || isShallow && r.sub != "" && r.sub == sub
	if !deep {
		return
	}
	if !sc.reads.named && sc.dir != "" {
		sc.reads.folders = append(sc.reads.folders, sc.dir)
	}
	for _, d := range sc.reads.folders {
		sc.below(d, levels)
	}
}

// into moves where the command is to the folder w names, a literal word, for the words
// after it (tar -C .. acme-api), and reports whether it did.
func (sc *scan) into(w *syntax.Word) bool {
	if p, ok := sc.path(w.Lit(), sc.dir); ok && w.Lit() != "" && sc.spend(1) {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			sc.prev, sc.dir = sc.dir, p
			return true
		}
	}
	return false
}

// runsShell reports whether cmd runs a shell, past its wrappers, reading its commands from
// its input: no -c, and no script file (bash -s and its arguments do).
func runsShell(cmd syntax.Command) bool {
	ce, ok := cmd.(*syntax.CallExpr)
	if !ok || len(ce.Args) == 0 {
		return false
	}
	u := unwrap(ce.Args)
	if len(u.command) == 0 {
		return false
	}
	s, ok := scriptOf(programName(u.command[0]))
	if !ok || !s.shell {
		return false
	}
	for _, w := range u.command[1:] {
		switch opt := w.Lit(); {
		case opt == "-s":
			return true
		case s.takes(opt), !strings.HasPrefix(opt, "-") && !strings.HasPrefix(opt, "+"):
			return false
		}
	}
	return true
}
