package hookrun

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/miradorlabs/terma-cli/internal/gitx"
)

// What ShellPaths knows of the commands it walks: those that run another (wrappers), run a
// script an option gives (scripts) or read every file below a folder (readers), and the
// options that take a folder or a value.

// wrapper is a command that runs the one its words name after its own: past its options,
// those in values taking the next word as theirs, NAME=value words and operands words
// (timeout's duration, flock's file). A chdir option's value is the folder it runs the
// command in (env -C), a split option's is split into the command's first words (env -S),
// and a shell option's is a shell script it runs (flock -c); a script wrapper runs its
// words as one (watch). One with a sub is a wrapper only when that word follows it (uv run).
type wrapper struct {
	sub                         string
	values, chdir, split, shell []string
	operands                    int
	script                      bool
}

// Commands by what they do with their words: wrappers run the command after their own;
// scripts run a script an option gives (scriptOf); folderOptions take a folder, as the next
// word or after their =, as -C does for dashC (for git only before its subcommand: git diff
// -C finds copies); valueOptions are a command's options before its subcommand that take
// the next word (git -c k=v log); readers read every file below a folder.
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
		"command":    {},
		"builtin":    {},
		"caffeinate": {values: []string{"-t", "-w"}},
		"xargs":      {values: []string{"-I", "-n", "-L", "-P", "-s", "-E", "-d", "-a", "--replace", "--max-args", "--max-lines", "--max-procs", "--max-chars", "--eof", "--delimiter", "--arg-file"}},
		"watch":      {values: []string{"-n", "--interval"}, script: true},
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
		"bash": shellScript, "sh": shellScript, "zsh": shellScript, "dash": shellScript, "ksh": shellScript, "ash": shellScript, "fish": shellScript,
		"python": {letters: "c"}, "pypy": {letters: "c"},
		"node": {letters: "ep", words: []string{"--eval", "--print"}}, "nodejs": {letters: "ep", words: []string{"--eval", "--print"}},
		"bun": {letters: "e", words: []string{"--eval", "--print"}}, "deno": {words: []string{"eval"}},
		"ruby": {letters: "e"}, "perl": {letters: "eE"}, "php": {letters: "r"},
	}
	folderOptions = []string{"--git-dir", "--work-tree", "--prefix", "--cwd", "--dir", "--directory"}
	dashC         = []string{"git", "make", "tar", "env"}
	valueOptions  = map[string][]string{"git": {"-c", "--config-env", "--namespace", "--super-prefix"}}
	grep          = reader{letters: "rR", words: []string{"--recursive", "--dereference-recursive", "--directories=recurse", "-d recurse"}}
	readers       = map[string]reader{
		"rg": {always: true}, "ag": {always: true}, "ack": {always: true}, "find": {always: true}, "fd": {always: true}, "tree": {always: true}, "rsync": {always: true},
		"grep": grep, "egrep": grep, "fgrep": grep, "zgrep": grep,
		"ls":  {letters: "R", words: []string{"--recursive"}},
		"cp":  {letters: "rRa", words: []string{"--recursive", "--archive"}},
		"tar": {letters: "c", words: []string{"--create"}, bare: true},
		"zip": {letters: "r", words: []string{"--recurse-paths"}},
		"git": {sub: "grep", words: []string{"--no-index"}},
	}
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

// reader is a command that reads every file below each folder it names: always, or with one
// of its options, a short one's letter in any cluster of them (grep -rn), with no dash in
// its first word when bare (tar czf), or a word, an option's value included (-d recurse);
// for one with a sub, only past that subcommand (git grep --no-index).
type reader struct {
	letters, sub string
	words        []string
	always, bare bool
}

// recursive reports whether opt, followed by next, the first of the command's words when
// first, makes r read below its folders.
func (r reader) recursive(opt, next string, first bool) bool {
	short := len(opt) > 1 && opt[0] == '-' && opt[1] != '-' || r.bare && first && opt != "" && opt[0] != '-'
	return short && strings.ContainsAny(strings.TrimPrefix(opt, "-"), r.letters) ||
		slices.Contains(r.words, opt) || slices.Contains(r.words, opt+" "+next)
}

// wrapped is a simple command's words past its wrappers: the wrappers' own (before) and the
// role each plays there (a folder option's value, a target for the folder one runs the
// command in, env -C's), a string one splits into the command's first words (split) or the
// shell script one runs instead (script), and the command, its program first.
type wrapped struct {
	before, script, command []*syntax.Word
	roles                   []role
	split                   *syntax.Word
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
			u.script, u.command = u.command, nil
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

// runsShell reports whether cmd runs a shell, past its wrappers, reading its commands from
// its input: no -c.
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
	return ok && s.shell && !slices.ContainsFunc(u.command[1:], func(w *syntax.Word) bool { return s.takes(w.Lit()) })
}

// maxBelowDepth bounds how far below a folder outside any checkout ShellPaths looks for the
// checkouts a recursive reader of it reads.
const maxBelowDepth = 4

// recurse reaches the checkouts below each folder a recursive reader reads: those its
// operands name, or where the command is when none names anything there (rg TODO).
func (sc *scan) recurse(operands []*syntax.Word) {
	var dirs []string
	named := false
	for _, w := range operands {
		fields, err := sc.fields(w)
		if err != nil {
			continue
		}
		for _, f := range fields {
			p, ok := sc.path(f, sc.dir)
			if !ok || !sc.spend(1) {
				continue
			}
			if info, err := os.Stat(p); err == nil {
				named = true
				if info.IsDir() {
					dirs = append(dirs, p)
				}
			}
		}
	}
	if !named && sc.dir != "" {
		dirs = append(dirs, sc.dir)
	}
	for _, dir := range dirs {
		sc.below(dir)
	}
}

// below reaches the checkouts below dir, a folder a recursive reader reads: none when dir is
// in a checkout, which is its place (one nested in it is not looked for); else each checkout
// a walk down at most maxBelowDepth levels finds, each folder it reads and each entry a
// lookup. A walk that runs out of either reaches somewhere not known.
func (sc *scan) below(dir string) {
	c := candidate{dir, "", walked}
	if _, seen := sc.examined[c]; seen || !sc.spend(1) {
		return
	}
	sc.examined[c] = ""
	if _, _, ok := gitx.LocateFS(dir); ok {
		return
	}
	level := []string{dir}
	for depth := 0; len(level) > 0 && !sc.unknown; depth++ {
		var next []string
		for _, d := range level {
			for _, e := range sc.readDir(d) {
				sub := filepath.Join(d, e.Name())
				if e.Type()&fs.ModeSymlink != 0 {
					if info, err := os.Stat(sub); err != nil || !info.IsDir() {
						continue
					}
				} else if !e.IsDir() {
					continue
				}
				if depth == maxBelowDepth || !sc.spend(1) {
					sc.unknown = true
					return
				}
				if _, err := os.Lstat(filepath.Join(sub, ".git")); err == nil {
					sc.place(sub)
					continue
				}
				next = append(next, sub)
			}
		}
		level = next
	}
}
