package hookrun

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

type wrapper struct {
	sub                                                string
	values, files, outputs, chdir, split, shell, names []string
	operands                                           int
	script, each, creates                              bool
}
type script struct {
	letters string
	words   []string
	shell   bool
}
type reader struct {
	letters, sub string
	words        []string
	bare         bool
}
type wrapped struct {
	before, script, command []*syntax.Word
	roles                   []role
	attached, envMod        map[int]string
	split                   *syntax.Word
	scriptPrefix            string
	each, unknown, xargs    bool
}

var wrappers = map[string]wrapper{
	"timeout": {values: []string{"-s", "--signal", "-k", "--kill-after"}, operands: 1}, "env": {chdir: []string{"-C", "--chdir"}, split: []string{"-S", "--split-string"}},
	"sudo": {values: []string{"-u", "--user", "-g", "--group", "-h", "--host", "-p", "--prompt", "-r", "--role", "-t", "--type", "-C", "--close-from", "-T", "--command-timeout", "-U", "--other-user"}, chdir: []string{"-D", "--chdir"}},
	"doas": {values: []string{"-u", "-C"}}, "nice": {values: []string{"-n", "--adjustment"}}, "ionice": {values: []string{"-c", "--class", "-n", "--classdata"}}, "stdbuf": {values: []string{"-i", "-o", "-e", "--input", "--output", "--error"}}, "nohup": {}, "builtin": {}, "exec": {values: []string{"-a"}}, "command": {names: []string{"-v", "-V"}},
	"time": {values: []string{"-f", "--format"}, outputs: []string{"-o", "--output"}}, "caffeinate": {values: []string{"-t", "-w"}}, "watch": {values: []string{"-n", "--interval"}, script: true}, "hyperfine": {script: true, each: true},
	"xargs": {values: []string{"-I", "-n", "-L", "-P", "-s", "-E", "-d", "--replace", "--max-args", "--max-lines", "--max-procs", "--max-chars", "--eof", "--delimiter"}, files: []string{"-a", "--arg-file"}},
	"flock": {values: []string{"-w", "--timeout", "-E", "--conflict-exit-code"}, shell: []string{"-c", "--command"}, operands: 1, creates: true}, "uv": {sub: "run", values: []string{"--with", "--python", "-p", "--package", "--extra", "--group", "--index"}, files: []string{"--env-file", "--with-requirements", "--config-file"}, chdir: []string{"--project", "--directory"}},
	"poetry": {sub: "run"}, "pipenv": {sub: "run"}, "bundle": {sub: "exec"}, "pnpm": {sub: "exec", values: []string{"--filter", "-F"}}, "npm": {sub: "exec", values: []string{"--package", "-w", "--workspace"}, shell: []string{"-c", "--call"}}, "npx": {values: []string{"-p", "--package"}, shell: []string{"-c", "--call"}},
}
var shellScript, folderOptions, dashC = script{letters: "c", words: []string{"--command"}, shell: true}, []string{"--git-dir", "--work-tree", "--prefix", "--cwd", "--dir", "--directory"}, []string{"git", "make", "tar", "env"}
var scripts, valueOptions, grep = map[string]script{"bash": shellScript, "sh": shellScript, "zsh": shellScript, "dash": shellScript, "ksh": shellScript, "ash": shellScript, "fish": shellScript, "su": shellScript, "python": {letters: "c"}, "pypy": {letters: "c"}, "ruby": {letters: "e"}, "perl": {letters: "eE"}, "php": {letters: "r"}, "deno": {words: []string{"eval"}}, "node": {letters: "ep", words: []string{"--eval", "--print"}}, "nodejs": {letters: "ep", words: []string{"--eval", "--print"}}, "bun": {letters: "e", words: []string{"--eval", "--print"}}}, map[string][]string{"git": {"-c", "--config-env", "--namespace", "--super-prefix"}}, reader{letters: "rR", words: []string{"--recursive", "--dereference-recursive", "--directories=recurse", "-d recurse"}}
var shallow = map[string]reader{
	"ls": {letters: "R", words: []string{"--recursive"}}, "git": {sub: "grep"}, "grep": grep, "egrep": grep, "fgrep": grep, "zgrep": grep, "cp": {letters: "rRa", words: []string{"--recursive", "--archive"}}, "tar": {letters: "c", words: []string{"--create"}, bare: true}, "zip": {letters: "r", words: []string{"--recurse-paths"}},
	"cd": {}, "pushd": {}, "popd": {}, "pwd": {}, "mkdir": {}, "rmdir": {}, "touch": {}, "mv": {}, "ln": {}, "stat": {}, "file": {}, "test": {}, "[": {}, "echo": {}, "printf": {}, "realpath": {}, "readlink": {}, "basename": {}, "dirname": {}, "which": {}, "command": {}, "true": {}, "false": {}, "export": {}, "declare": {}, "typeset": {}, "local": {}, "readonly": {}, "unset": {},
}

func (s script) takes(opt string) bool {
	return scriptFlag(opt, s.letters, s.shell) || slices.Contains(s.words, opt)
}
func scriptOf(name string) (script, bool) {
	s, ok := scripts[strings.TrimRight(name, "0123456789.")]
	return s, ok
}
func (r reader) recursive(opt, next string, first bool) bool {
	short := len(opt) > 1 && opt[0] == '-' && opt[1] != '-' || r.bare && first && opt != "" && opt[0] != '-'
	return short && r.letters != "" && strings.ContainsAny(strings.TrimPrefix(opt, "-"), r.letters) || slices.Contains(r.words, opt) || slices.Contains(r.words, opt+" "+next)
}

// shellAt is the index among args of a shell given its script with -c past its options, or -1.
func shellAt(args []*syntax.Word) int {
	for k, w := range args {
		if s, ok := scriptOf(programName(w)); ok && s.shell {
			for _, o := range args[k+1:] {
				if s.takes(o.Lit()) {
					return k
				} else if !strings.HasPrefix(o.Lit(), "-") {
					break
				}
			}
		}
	}
	return -1
}
func (w wrapper) optRole(word *syntax.Word, opt string, value bool) (string, role, bool) {
	for i, opts := range [][]string{w.chdir, w.files, w.outputs, w.values} {
		r := []role{target, plain, creates, program}[i]
		if value && slices.Contains(opts, opt) {
			return "", r, true
		} else if m := attachedOpt(word, opts); m != "" {
			return m, pick(r == target, folder, r), false
		}
	}
	return "", 0, false
}
func unwrap(args []*syntax.Word) wrapped {
	u := wrapped{command: args, attached: map[int]string{}, envMod: map[int]string{}}
	for len(u.command) > 1 {
		prog := programName(u.command[0])
		w, ok := wrappers[prog]
		n := pick(w.sub != "", 2, 1)
		if !ok || w.sub != "" && u.command[1].Lit() != w.sub {
			break
		}
		roles := slices.Repeat([]role{program}, n)
		u.xargs = u.xargs || prog == "xargs"
	options:
		for operands := 0; n < len(u.command); n++ {
			word, value := u.command[n], n+1 < len(u.command)
			opt, lead, idx := word.Lit(), leadingLit(word), len(u.before)+len(roles)
			if slices.Contains(w.names, opt) {
				return u // command -v runs nothing
			} else if opt == "--" {
				n, roles = n+1, append(roles, program)
				break options
			} else if value && slices.Contains(w.split, opt) {
				u.before, u.roles, u.split, u.command, u.unknown = append(u.before, u.command[:n+1]...), append(append(u.roles, roles...), program), u.command[n+1], u.command[n+2:], u.unknown || u.xargs
				return u
			} else if value && slices.Contains(w.shell, opt) {
				u.before, u.roles, u.script, u.command, u.unknown = append(u.before, u.command[:n+1]...), append(append(u.roles, roles...), program), u.command[n+1:n+2], nil, u.unknown || u.xargs
				return u
			} else if m := attachedOpt(word, w.shell); m != "" {
				u.before, u.roles, u.script, u.scriptPrefix, u.command, u.unknown = append(u.before, u.command[:n]...), append(u.roles, roles...), u.command[n:n+1], m+pick(strings.HasPrefix(m, "--"), "=", ""), nil, u.unknown || u.xargs
				return u
			} else if m, r, next := w.optRole(word, opt, value); next {
				u.unknown, n, roles = u.unknown || prog == "xargs" && r == plain, n+1, append(roles, program, r)
			} else if m != "" {
				u.unknown, roles = u.unknown || prog == "xargs" && r == plain, append(roles, r)
				u.attached[idx] = m
			} else if m := attachedOpt(word, []string{"-u", "--unset"}); prog == "env" && (m != "" || opt == "-" || opt == "-i" || opt == "--ignore-environment" || value && (opt == "-u" || opt == "--unset")) {
				if opt == "-u" || opt == "--unset" {
					u.envMod[idx+1] = ""
					n, roles = n+1, append(roles, program)
				} else {
					u.envMod[idx] = pick(m != "", m+pick(strings.HasPrefix(m, "--"), "=", ""), "-i")
				}
				roles = append(roles, program)
			} else if prog == "xargs" && isShortFlag(lead) && strings.ContainsRune(lead[1:], 'a') || prog == "env" && (strings.HasPrefix(lead, "--split-string=") || isShortFlag(lead) && len(lead) > 2 && strings.ContainsAny(lead[1:], "iuCS")) {
				u.unknown, roles = true, append(roles, program)
			} else if strings.HasPrefix(lead, "-") && lead != "-" || isAssignment(word) {
				roles = append(roles, program)
			} else if operands < w.operands {
				operands, roles = operands+1, append(roles, pick(w.creates && (opt == "" || strings.Trim(opt, "0123456789") != ""), plain, program))
			} else {
				break options
			}
		}
		if u.before, u.roles, u.command = append(u.before, u.command[:n]...), append(u.roles, roles...), u.command[n:]; w.script {
			u.script, u.command, u.each, u.unknown = u.command, nil, w.each, u.unknown || u.xargs
		}
	}
	u.unknown = u.unknown || u.xargs && len(u.command) > 0 && isScriptCmd(programName(u.command[0]))
	return u
}
func isScriptCmd(p string) bool {
	_, s := scriptOf(p)
	return s || slices.Contains([]string{"eval", "exec", "source", ".", "cd", "pushd"}, p)
}
func isShortFlag(s string) bool { return strings.HasPrefix(s, "-") && !strings.HasPrefix(s, "--") }
func leadingLit(w *syntax.Word) string {
	if lead, ok := pick(len(w.Parts) > 0, w.Parts, []syntax.WordPart{nil})[0].(*syntax.Lit); ok {
		return lead.Value
	}
	return ""
}
func attachedOpt(w *syntax.Word, opts []string) string {
	lead := leadingLit(w)
	for _, opt := range opts {
		if strings.HasPrefix(opt, "--") && strings.HasPrefix(lead, opt+"=") || !strings.HasPrefix(opt, "--") && strings.HasPrefix(lead, opt) && w.Lit() != opt {
			return opt
		}
	}
	return ""
}
func isAssignment(w *syntax.Word) bool {
	name, _, eq := strings.Cut(leadingLit(w), "=")
	return eq && isName(name)
}
func programName(w *syntax.Word) string {
	return pick(w.Lit() != "", strings.TrimSuffix(filepath.Base(w.Lit()), ".exe"), "")
}
func scriptFlag(opt, letters string, anywhere bool) bool {
	return len(opt) >= 2 && opt[0] == '-' && !strings.ContainsFunc(opt[1:], func(r rune) bool { return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') }) && (anywhere && strings.ContainsAny(opt[1:], letters) || !anywhere && len(opt) <= 4 && strings.ContainsRune(letters, rune(opt[len(opt)-1])))
}
func folderValue(w *syntax.Word, dashCFolder bool) role {
	opt, _, eq := strings.Cut(leadingLit(w), "=")
	return pick(eq && slices.Contains(folderOptions, opt) || dashCFolder && strings.HasPrefix(opt, "-C") && w.Lit() != "-C", folder, plain)
}

// run reads a command past its wrappers, tracking -C/--directory moves, inline scripts,
// depth bounds (ignoring depth limits for find/fd -exec), and downward folder reads.
func (sc *scan) run(args []*syntax.Word) {
	sc.arg(args[0], program)
	lit, name, args := sc.command(args)
	if body, isFn := sc.funcs[lit]; isFn && lit != "" {
		if sc.unknown = sc.unknown || len(args) > 0 || body == nil || sc.depth >= maxShellDepth; !sc.unknown {
			sc.depth++
			sc.stmt(body, false)
			sc.depth--
		}
		return
	}
	switch name {
	case "cd", "pushd":
		sc.cd(args, name == "pushd")
	case "popd":
		if n := len(sc.pushed); n > 0 {
			sc.prev, sc.dir, sc.pushed = sc.dir, sc.pushed[n-1], sc.pushed[:n-1]
		}
	case "eval", "trap":
		args = args[pick(len(args) > 0 && args[0].Lit() == "--", 1, 0):]
		sc.inline(pick(name == "trap", args[:min(len(args), 1)], args), "", true, name == "trap")
	case "source", ".":
		for _, w := range args {
			sc.arg(w, plain)
		}
		sc.unknown = sc.unknown || len(args) > 0
	case "unset":
		sc.unset(args)
	case "read", "mapfile", "readarray":
		sc.readBuiltin(args, name != "read")
	case "export", "declare", "typeset", "local", "readonly":
		for _, w := range args {
			if isAssignment(w) {
				sc.export(w)
			} else if lit := w.Lit(); !strings.HasPrefix(lit, "-") || strings.ContainsRune(lit, 'n') { // -n: a nameref, no name, fails closed
				if name == "local" || name == "declare" || name == "typeset" {
					sc.invalidateName(lit)
				}
				sc.arg(w, plain)
			}
		}
	default:
		sc.runExternal(name, args)
	}
}

// command resolves an expanded, escaped or braced command word, failing closed unless one naming no wrapper or interpreter.
func (sc *scan) command(args []*syntax.Word) (lit, name string, rest []*syntax.Word) {
	if lit, rest = args[0].Lit(), args[1:]; lit != "" && !strings.ContainsAny(lit, `\{`) {
		return lit, programName(args[0]), rest
	}
	f, err := sc.fields(args[0])
	lit = pick(err == nil && len(f) == 1, strings.Join(f, ""), "")
	name = pick(lit != "", strings.TrimSuffix(filepath.Base(lit), ".exe"), "")
	_, wrapped := wrappers[name]
	sc.unknown = sc.unknown || lit == "" || wrapped || isScriptCmd(name)
	return lit, name, rest
}
func isFindExec(name, opt string) bool {
	return name == "find" && slices.Contains([]string{"-exec", "-execdir", "-ok", "-okdir"}, opt) || name == "fd" && (slices.Contains([]string{"-x", "--exec", "-X", "--exec-batch"}, opt) || strings.HasPrefix(opt, "--exec=") || strings.HasPrefix(opt, "--exec-batch="))
}
func (sc *scan) runExternal(name string, args []*syntax.Word) {
	s, scripted := scriptOf(name)
	r, isShallow := shallow[name]
	if k := shellAt(args); k >= 0 && !isShallow && !slices.Contains([]string{"rg", "ag", "ack"}, name) {
		if name != "find" && name != "fd" {
			for _, w := range args[:k] {
				sc.arg(w, plain)
			}
			sc.run(args[k:])
			return
		} else if slices.ContainsFunc(args[:k], func(w *syntax.Word) bool { return isFindExec(name, w.Lit()) }) {
			sc.run(args[k:])
		}
	}
	deep, sub, levels, hasExec, hasInline, skipScript, endOpts, outer, dir, prev, moved := !isShallow, "", maxBelowDepth+2, false, false, false, false, sc.reads, sc.dir, sc.prev, false
	sc.reads = &cached{}
	defer func() {
		if sc.reads = outer; moved {
			sc.dir, sc.prev = dir, prev
		}
	}()
	for j := 0; j < len(args); j++ {
		opt, next := args[j].Lit(), j+1 < len(args)
		after := pick(next, args[min(j+1, len(args)-1)].Lit(), "")
		dashCFolder := slices.Contains(dashC, name) && (name != "git" || sub == "")
		hasExec = hasExec || isFindExec(name, opt)
		switch {
		case name == "printf" && sub == "" && strings.HasPrefix(leadingLit(args[j]), "-v"):
			if opt == "-v" && next {
				j++
				sc.invalidateVar(args[j])
			} else {
				sc.invalidateName(pick(opt != "", strings.TrimPrefix(opt, "-v"), ""))
			}
		case next && scripted && !endOpts && s.takes(opt):
			hasInline = true
			j++
			sc.inline(args[j:j+1], "", s.shell, true)
		case scripted && !hasInline && !skipScript && !endOpts && opt == "--":
			endOpts = true
		case next && scripted && !hasInline && !skipScript && !endOpts && (s.shell && slices.Contains([]string{"-o", "+o", "-O", "+O"}, opt) || !s.shell && slices.Contains([]string{"-m", "-W", "-X", "-r", "--require", "-I", "-C"}, opt)):
			skipScript = skipScript || opt == "-m"
			j++
			sc.arg(args[j], plain)
			sc.unknown = sc.unknown || !s.shell && (opt == "-r" || opt == "--require") && sc.reads.named
		case next && (slices.Contains(folderOptions, opt) || opt == "-C" && dashCFolder):
			j++
			sc.arg(args[j], folder)
			if (opt == "-C" || opt == "--directory") && name != "env" {
				if fields, err := sc.fields(args[j]); err != nil {
					sc.unknown = true
				} else {
					moved = len(fields) == 1 && sc.intoPath(fields[0]) || moved
				}
			}
		case next && sub == "" && slices.Contains(valueOptions[name], opt):
			j++
			sc.arg(args[j], plain)
		case next && (opt == "-maxdepth" || opt == "--max-depth" || name == "fd" && opt == "-d" || name == "tree" && opt == "-L"):
			j++
			if n, err := strconv.Atoi(after); err == nil {
				levels = n
			}
		default:
			deep = deep || isShallow && r.recursive(opt, after, j == 0)
			if n, err := strconv.Atoi(strings.TrimPrefix(opt, "--max-depth=")); strings.HasPrefix(opt, "--max-depth=") && err == nil {
				levels = n
			}
			sub = pick(sub == "" && !strings.HasPrefix(opt, "-"), opt, sub)
			sc.arg(args[j], folderValue(args[j], dashCFolder))
			if scripted && !hasInline && !skipScript {
				if !endOpts && (strings.HasPrefix(opt, "-") || s.shell && strings.HasPrefix(opt, "+")) && opt != "-" {
					skipScript = s.shell && opt[0] == '-' && !strings.HasPrefix(opt, "--") && strings.ContainsRune(opt[1:], 's')
				} else if opt != "run" || name != "deno" && name != "bun" {
					skipScript, sc.unknown = true, sc.unknown || s.shell || sc.reads.named
				}
			}
		}
	}
	if deep || isShallow && r.sub != "" && r.sub == sub {
		levels = pick(hasExec, maxBelowDepth+2, levels)
		for _, d := range pick(!sc.reads.named && sc.dir != "", append(sc.reads.folders, sc.dir), sc.reads.folders) {
			sc.below(d, levels)
		}
	}
}
func (sc *scan) intoPath(val string) bool {
	if p, ok := sc.path(val, sc.dir); ok && val != "" && sc.spend(1) {
		if r, _, info, found := sc.walkPath(p, true); found && info.IsDir() {
			sc.prev, sc.dir = sc.dir, r
			return true
		}
	}
	return false
}
func (sc *scan) readBuiltin(args []*syntax.Word, mapfile bool) {
	opts, named, valFlags, boolFlags := true, false, pick(mapfile, "dnOsucC", "dinNptu"), pick(mapfile, "t", "ers")
	for i := 0; i < len(args); i++ {
		sc.substitutions(args[i])
		if lit := args[i].Lit(); opts && lit == "--" {
			opts = false
		} else if opts && strings.HasPrefix(leadingLit(args[i]), "-") && lit != "-" {
			sc.unknown = sc.unknown || lit == ""
			for k := 1; k < len(lit); k++ {
				if c := lit[k]; !mapfile && c == 'a' || strings.IndexByte(valFlags, c) >= 0 {
					sc.unknown = sc.unknown || mapfile && c == 'C'
					if named = named || !mapfile && c == 'a'; k+1 < len(lit) && !mapfile && c == 'a' {
						sc.invalidateName(lit[k+1:])
					} else if k+1 == len(lit) && i+1 < len(args) {
						i++
						sc.substitutions(args[i])
						if !mapfile && c == 'a' {
							sc.invalidateVar(args[i])
						}
					} else {
						sc.unknown = sc.unknown || !mapfile && c == 'a'
					}
					break
				} else if strings.IndexByte(boolFlags, c) < 0 {
					sc.unknown = true
				}
			}
		} else {
			opts, named = false, true
			sc.invalidateVar(args[i])
		}
	}
	if !named {
		sc.setVar(pick(mapfile, "MAPFILE", "REPLY"), nil)
	}
}
func (sc *scan) invalidateName(s string) {
	if base, _, _ := strings.Cut(s, "["); isName(base) {
		sc.setVar(base, nil)
	} else {
		sc.unknown = true
	}
}
func (sc *scan) invalidateVar(w *syntax.Word) {
	fields, err := sc.fields(w)
	for _, f := range fields {
		sc.invalidateName(f)
	}
	sc.unknown = sc.unknown || err != nil || len(fields) == 0
}
func (sc *scan) unset(args []*syntax.Word) {
	for opts, fn := true, false; len(args) > 0; args = args[1:] {
		if lit := args[0].Lit(); opts && (lit == "--" || len(lit) > 1 && lit[0] == '-') {
			opts, fn = lit != "--", fn || strings.ContainsRune(lit[1:], 'f')
			continue
		}
		opts = false
		sc.substitutions(args[0])
		fields, err := sc.fields(args[0])
		if err != nil && sc.fail() {
			return
		}
		for _, f := range fields {
			if fn {
				delete(sc.funcs, f)
			} else if base, _, hasIdx := strings.Cut(f, "["); isName(base) {
				sc.setVar(base, pick(hasIdx, nil, unsetVar))
			}
		}
	}
}
