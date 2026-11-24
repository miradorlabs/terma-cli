package hookrun

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// wrapper is a command that runs the one its words name after its own options/operands.
type wrapper struct {
	sub                                string
	values, chdir, split, shell, names []string
	operands                           int
	script, each                       bool
}

var (
	wrappers = map[string]wrapper{
		"timeout": {values: []string{"-s", "--signal", "-k", "--kill-after"}, operands: 1},
		"env":     {values: []string{"-u", "--unset"}, chdir: []string{"-C", "--chdir"}, split: []string{"-S", "--split-string"}},
		"sudo":    {values: []string{"-u", "--user", "-g", "--group", "-h", "--host", "-p", "--prompt", "-r", "--role", "-t", "--type", "-C", "--close-from", "-T", "--command-timeout", "-U", "--other-user"}, chdir: []string{"-D", "--chdir"}},
		"doas":    {values: []string{"-u", "-C"}}, "nice": {values: []string{"-n", "--adjustment"}},
		"ionice": {values: []string{"-c", "--class", "-n", "--classdata"}}, "stdbuf": {values: []string{"-i", "-o", "-e", "--input", "--output", "--error"}},
		"nohup": {}, "builtin": {}, "exec": {values: []string{"-a"}}, "command": {names: []string{"-v", "-V"}},
		"time": {values: []string{"-f", "--format", "-o", "--output"}}, "caffeinate": {values: []string{"-t", "-w"}},
		"xargs": {values: []string{"-I", "-n", "-L", "-P", "-s", "-E", "-d", "-a", "--replace", "--max-args", "--max-lines", "--max-procs", "--max-chars", "--eof", "--delimiter", "--arg-file"}},
		"watch": {values: []string{"-n", "--interval"}, script: true}, "hyperfine": {script: true, each: true},
		"flock":  {values: []string{"-w", "--timeout", "-E", "--conflict-exit-code"}, shell: []string{"-c", "--command"}, operands: 1},
		"uv":     {sub: "run", values: []string{"--with", "--python", "-p", "--package", "--extra", "--group", "--env-file", "--index"}, chdir: []string{"--project", "--directory"}},
		"poetry": {sub: "run"}, "pipenv": {sub: "run"}, "bundle": {sub: "exec"}, "pnpm": {sub: "exec", values: []string{"--filter", "-F"}},
		"npm": {sub: "exec", values: []string{"--package", "-w", "--workspace"}, shell: []string{"-c", "--call"}}, "npx": {values: []string{"-p", "--package"}, shell: []string{"-c", "--call"}},
	}
	shellScript = script{letters: "c", words: []string{"--command"}, shell: true}
	scripts     = map[string]script{
		"bash": shellScript, "sh": shellScript, "zsh": shellScript, "dash": shellScript, "ksh": shellScript, "ash": shellScript, "fish": shellScript, "su": shellScript,
		"python": {letters: "c"}, "pypy": {letters: "c"}, "ruby": {letters: "e"}, "perl": {letters: "eE"}, "php": {letters: "r"},
		"node": {letters: "ep", words: []string{"--eval", "--print"}}, "nodejs": {letters: "ep", words: []string{"--eval", "--print"}},
		"bun": {letters: "e", words: []string{"--eval", "--print"}}, "deno": {words: []string{"eval"}},
	}
	folderOptions = []string{"--git-dir", "--work-tree", "--prefix", "--cwd", "--dir", "--directory"}
	dashC         = []string{"git", "make", "tar", "env"}
	valueOptions  = map[string][]string{"git": {"-c", "--config-env", "--namespace", "--super-prefix"}}
	grep          = reader{letters: "rR", words: []string{"--recursive", "--dereference-recursive", "--directories=recurse", "-d recurse"}}
	shallow       = map[string]reader{
		"ls": {letters: "R", words: []string{"--recursive"}}, "git": {sub: "grep"}, "grep": grep, "egrep": grep, "fgrep": grep, "zgrep": grep,
		"cp":  {letters: "rRa", words: []string{"--recursive", "--archive"}},
		"tar": {letters: "c", words: []string{"--create"}, bare: true}, "zip": {letters: "r", words: []string{"--recurse-paths"}},
		"cd": {}, "pushd": {}, "popd": {}, "pwd": {}, "mkdir": {}, "rmdir": {}, "touch": {}, "mv": {}, "ln": {},
		"stat": {}, "file": {}, "test": {}, "[": {}, "echo": {}, "printf": {}, "realpath": {}, "readlink": {},
		"basename": {}, "dirname": {}, "which": {}, "command": {}, "true": {}, "false": {},
		"export": {}, "declare": {}, "typeset": {}, "local": {}, "readonly": {}, "unset": {},
	}
	depthOptions = map[string]string{"fd": "-d", "tree": "-L"}
)

func depthOption(name, opt string) bool {
	return opt == "-maxdepth" || opt == "--max-depth" || depthOptions[name] == opt && opt != ""
}
func execAction(name, opt string) bool {
	if name == "find" {
		return opt == "-exec" || opt == "-execdir" || opt == "-ok" || opt == "-okdir"
	}
	return name == "fd" && (opt == "-x" || opt == "--exec" || opt == "-X" || opt == "--exec-batch" ||
		strings.HasPrefix(opt, "--exec=") || strings.HasPrefix(opt, "--exec-batch="))
}

type script struct {
	letters string
	words   []string
	shell   bool
}

func (s script) takes(opt string) bool {
	return scriptFlag(opt, s.letters, s.shell) || slices.Contains(s.words, opt)
}
func scriptOf(name string) (script, bool) {
	s, ok := scripts[strings.TrimRight(name, "0123456789.")]
	return s, ok
}

type reader struct {
	letters, sub string
	words        []string
	bare         bool
}

func (r reader) recursive(opt, next string, first bool) bool {
	short := len(opt) > 1 && opt[0] == '-' && opt[1] != '-' || r.bare && first && opt != "" && opt[0] != '-'
	return short && r.letters != "" && strings.ContainsAny(strings.TrimPrefix(opt, "-"), r.letters) ||
		slices.Contains(r.words, opt) || slices.Contains(r.words, opt+" "+next)
}

// shellAt is the index among args of a shell given its script with -c past its options, or -1.
func shellAt(args []*syntax.Word) int {
	for k, w := range args {
		if s, ok := scriptOf(programName(w)); ok && s.shell {
			for _, o := range args[k+1:] {
				if s.takes(o.Lit()) {
					return k
				}
				if !strings.HasPrefix(o.Lit(), "-") {
					break
				}
			}
		}
	}
	return -1
}

type wrapped struct {
	before, script, command []*syntax.Word
	roles                   []role
	attachedChdir           map[int]string
	split                   *syntax.Word
	each                    bool
}

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
				n, roles = n+1, append(roles, program)
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
				n, roles = n+1, append(roles, program, target)
			case value && slices.Contains(w.values, opt):
				n, roles = n+1, append(roles, program, program)
			case attachedOpt(word, w.chdir) != "":
				if u.attachedChdir == nil {
					u.attachedChdir = map[int]string{}
				}
				u.attachedChdir[len(u.before)+len(roles)] = attachedOpt(word, w.chdir)
				roles = append(roles, folder)
			case strings.HasPrefix(lead, "-") && lead != "-", isAssignment(word):
				roles = append(roles, program)
			case operands < w.operands:
				operands, roles = operands+1, append(roles, program)
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
func leadingLit(w *syntax.Word) string {
	if len(w.Parts) > 0 {
		if lead, ok := w.Parts[0].(*syntax.Lit); ok {
			return lead.Value
		}
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
	if name := w.Lit(); name != "" {
		return strings.TrimSuffix(filepath.Base(name), ".exe")
	}
	return ""
}
func scriptFlag(opt, letters string, anywhere bool) bool {
	if len(opt) < 2 || opt[0] != '-' || strings.ContainsFunc(opt[1:], func(r rune) bool { return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') }) {
		return false
	}
	if anywhere {
		return strings.ContainsAny(opt[1:], letters)
	}
	return len(opt) <= 4 && strings.ContainsRune(letters, rune(opt[len(opt)-1]))
}
func folderValue(w *syntax.Word, dashCFolder bool) role {
	if opt, _, eq := strings.Cut(leadingLit(w), "="); eq && slices.Contains(folderOptions, opt) || dashCFolder && strings.HasPrefix(opt, "-C") && w.Lit() != "-C" {
		return folder
	}
	return plain
}

// run reads a command past its wrappers, tracking -C/--directory moves, inline scripts,
// depth bounds (ignoring depth limits for find/fd -exec), and downward folder reads.
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
	case "eval", "trap":
		if name == "trap" {
			args = args[:min(len(args), 1)]
		}
		sc.inline(args, true)
		return
	case "export", "declare", "typeset", "local", "readonly":
		for _, w := range args {
			if isAssignment(w) {
				sc.export(w)
			} else if !strings.HasPrefix(w.Lit(), "-") {
				sc.arg(w, plain)
			}
		}
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
	deep, sub, levels, hasExec := !isShallow, "", maxBelowDepth+2, false
	outer, dir, prev, moved := sc.reads, sc.dir, sc.prev, false
	sc.reads = &operands{}
	defer func() {
		if sc.reads = outer; moved {
			sc.dir, sc.prev = dir, prev
		}
	}()
	for j := 0; j < len(args); j++ {
		opt, next, after := args[j].Lit(), j+1 < len(args), ""
		if next {
			after = args[j+1].Lit()
		}
		dashCFolder := slices.Contains(dashC, name) && (name != "git" || sub == "")
		hasExec = hasExec || execAction(name, opt)
		switch {
		case next && scripted && s.takes(opt):
			j++
			sc.inline(args[j:j+1], s.shell)
		case next && (slices.Contains(folderOptions, opt) || opt == "-C" && dashCFolder):
			j++
			sc.arg(args[j], folder)
			if (opt == "-C" || opt == "--directory") && name != "env" {
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
	if hasExec {
		levels = maxBelowDepth + 2
	}
	if !deep && (!isShallow || r.sub == "" || r.sub != sub) {
		return
	}
	if !sc.reads.named && sc.dir != "" {
		sc.reads.folders = append(sc.reads.folders, sc.dir)
	}
	for _, d := range sc.reads.folders {
		sc.below(d, levels)
	}
}
func (sc *scan) into(w *syntax.Word) bool {
	fields, err := sc.fields(w)
	if err != nil {
		sc.unknown = true
		return false
	}
	return len(fields) == 1 && sc.intoPath(fields[0])
}
func (sc *scan) intoPath(val string) bool {
	if p, ok := sc.path(val, sc.dir); ok && val != "" && sc.spend(1) && !sc.hasRemoteLink(p) {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			sc.prev, sc.dir = sc.dir, p
			return true
		}
	}
	return false
}
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
	for i, args := 0, u.command[1:]; i < len(args); i++ {
		switch opt := args[i].Lit(); {
		case opt == "-s":
			return true
		case opt == "-o" || opt == "+o" || opt == "-O" || opt == "+O":
			i++
		case s.takes(opt), !strings.HasPrefix(opt, "-") && !strings.HasPrefix(opt, "+"):
			return false
		}
	}
	return true
}
