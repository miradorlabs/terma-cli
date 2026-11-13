package hookrun

import (
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// shellWord is one word of a shell command as ShellPaths reads it: its parts, as quoted
// and expanded, and where it stands.
type shellWord struct {
	parts []wordPart
	// command: the word names the command to run, which the shell looks up on PATH when it
	// holds no slash, never in the cwd.
	command bool
	// body: the word is in a here-document's body, which a command reads as data unless it
	// is a shell; only a word holding a slash may be a path there.
	body bool
}

// partKind is how a part of a shell word reads.
type partKind int

const (
	// literal is text as written, unquoted: a glob's wildcards are live in it.
	literal partKind = iota
	// quoted is text from quotes or an escape, taken as it is.
	quoted
	// variable is a parameter expansion, $NAME or ${NAME}, text its name and fallback its
	// ${NAME:-fallback}; $(pwd) and `pwd` are PWD.
	variable
	// opaque is an expansion ShellPaths cannot resolve: a command substitution's output,
	// arithmetic, a positional or special parameter, or a parameter's operator.
	opaque
)

// wordPart is one part of a shell word.
type wordPart struct {
	kind     partKind
	text     string
	fallback *string
}

// literalText is the word as written when it is plain, unquoted, unexpanded text; false
// otherwise.
func (w shellWord) literalText() (string, bool) {
	var b strings.Builder
	for _, p := range w.parts {
		if p.kind != literal {
			return "", false
		}
		b.WriteString(p.text)
	}
	return b.String(), true
}

// shellCommands are the commands that run a here-document's body as shell commands.
var shellCommands = []string{"bash", "sh", "zsh", "dash", "ksh", "ash", "fish"}

// keepsCommand are the words after which the next word still names a command.
var keepsCommand = []string{"!", "{", "if", "then", "else", "elif", "do", "while", "until", "time", "builtin", "command", "exec", "nohup"}

// shellWords splits command into words outside quotes: at whitespace and newlines, at ;, &
// and |, at < and > (so &&, ||, >>, 2>&1), and around subshells and command substitutions,
// those inside double quotes too, whose words come before the word that holds them. Quotes
// and escaping backslashes are dropped, and what they held is marked quoted. A
// here-document's body is read only when its command is a shell or its delimiter is
// unquoted, its words then marked body unless that command is a shell.
func shellWords(command string, escapes bool) []shellWord {
	s := &scanner{rs: []rune(command), escapes: escapes}
	s.commands(0)
	return s.words
}

// scanner reads a command, rune by rune.
type scanner struct {
	rs    []rune
	i     int
	words []shellWord
	// heredocs wait for the next newline, where their bodies begin.
	heredocs []heredoc
	// body marks every word read as a here-document's body.
	body bool
	// literal reads $ and ` as text: a body whose delimiter was quoted expands nothing.
	literal bool
	// escapes is whether a backslash escapes the next character, as in a POSIX shell; else
	// it is literal, as in PowerShell and cmd, where it separates a Windows path.
	escapes bool
}

// heredoc is a here-document still to be read: its delimiter, whether that was quoted (its
// body is then data, nothing expanded), whether leading tabs are stripped, and whether
// its command is a shell.
type heredoc struct {
	delim         string
	quoted, strip bool
	shell         bool
}

func (s *scanner) peek(off int) rune {
	if s.i+off < len(s.rs) {
		return s.rs[s.i+off]
	}
	return 0
}

// commands reads commands until closer, or the end for 0, which it consumes.
func (s *scanner) commands(closer rune) {
	atCommand, redirect := true, false
	name := "" // the current simple command's name, for a here-document
	for s.i < len(s.rs) {
		c := s.rs[s.i]
		switch {
		case closer != 0 && c == closer:
			s.i++
			return
		case c == '\n':
			s.i++
			s.bodies()
			atCommand, redirect, name = true, false, ""
		case unicode.IsSpace(c):
			s.i++
		case c == ';' || c == '&' || c == '|':
			s.i++
			atCommand, redirect, name = true, false, ""
		case c == '(':
			s.i++
			s.commands(')')
			atCommand, redirect = false, false
		case c == ')':
			s.i++ // one that closes nothing
		case c == '<' && s.peek(1) == '<' && s.peek(2) != '<':
			s.i += 2
			s.heredoc(name)
		case c == '<' || c == '>':
			for s.i < len(s.rs) && strings.ContainsRune("<>&|", s.rs[s.i]) {
				s.i++
			}
			redirect = true
		default:
			w := s.word(closer)
			w.command = atCommand && !redirect
			s.words = append(s.words, w)
			switch text, plain := w.literalText(); {
			case redirect:
				redirect = false
			case !atCommand:
			case plain && slices.Contains(keepsCommand, text):
			case isAssignment(w):
			default:
				atCommand, name = false, text
			}
		}
	}
}

// heredoc reads a here-document's operator past its <<, and its delimiter, whose body
// begins at the next newline.
func (s *scanner) heredoc(command string) {
	h := heredoc{shell: slices.Contains(shellCommands, filepath.Base(filepath.ToSlash(command)))}
	if s.peek(0) == '-' {
		h.strip = true
		s.i++
	}
	for s.i < len(s.rs) && (s.rs[s.i] == ' ' || s.rs[s.i] == '\t') {
		s.i++
	}
	for _, p := range s.word(0).parts {
		h.delim += p.text
		h.quoted = h.quoted || p.kind == quoted
	}
	if h.delim != "" {
		s.heredocs = append(s.heredocs, h)
	}
}

// bodies reads the bodies of the here-documents waiting for this newline, in order: as
// commands for a shell, else as data, where a word holding a slash may still be a path
// (piped to a shell, say). A quoted delimiter's body expands nothing.
func (s *scanner) bodies() {
	pending := s.heredocs
	s.heredocs = nil
	for _, h := range pending {
		var lines []string
		for s.i < len(s.rs) {
			end := slices.Index(s.rs[s.i:], '\n')
			if end < 0 {
				end = len(s.rs) - s.i
			}
			line := string(s.rs[s.i : s.i+end])
			s.i = min(s.i+end+1, len(s.rs))
			if h.strip {
				line = strings.TrimLeft(line, "\t")
			}
			if line == h.delim {
				break
			}
			lines = append(lines, line)
		}
		body := &scanner{rs: []rune(strings.Join(lines, "\n")), body: s.body || !h.shell, literal: h.quoted && !h.shell, escapes: s.escapes}
		body.commands(0)
		s.words = append(s.words, body.words...)
	}
}

// word reads one word, up to whitespace, an operator, or closer.
func (s *scanner) word(closer rune) shellWord {
	w := shellWord{body: s.body}
	add := func(kind partKind, text string) {
		if n := len(w.parts); n > 0 && w.parts[n-1].kind == kind && kind != variable && kind != opaque {
			w.parts[n-1].text += text
			return
		}
		w.parts = append(w.parts, wordPart{kind: kind, text: text})
	}
	for s.i < len(s.rs) {
		c := s.rs[s.i]
		switch {
		case c == closer || unicode.IsSpace(c) || strings.ContainsRune(";&|<>()", c):
			return w
		case c == '\'':
			s.i++
			end := slices.Index(s.rs[s.i:], '\'')
			if end < 0 {
				end = len(s.rs) - s.i
			}
			add(quoted, string(s.rs[s.i:s.i+end]))
			s.i = min(s.i+end+1, len(s.rs))
		case c == '"':
			s.i++
			s.doubleQuoted(&w, add)
		case c == '\\' && s.escapes:
			switch next := s.peek(1); next {
			case 0:
				s.i++
			case '\n':
				s.i += 2 // a line continued
			default:
				add(quoted, string(next))
				s.i += 2
			}
		case s.literal && (c == '$' || c == '`'):
			add(quoted, string(c))
			s.i++
		case c == '$':
			s.dollar(&w, add)
		case c == '`':
			s.i++
			w.parts = append(w.parts, s.substitution('`'))
		default:
			add(literal, string(c))
			s.i++
		}
	}
	return w
}

// doubleQuoted reads the inside of double quotes into w: a backslash escapes only what is
// special there, and expansions and substitutions stay live.
func (s *scanner) doubleQuoted(w *shellWord, add func(partKind, string)) {
	for s.i < len(s.rs) {
		c := s.rs[s.i]
		switch {
		case c == '"':
			s.i++
			return
		case c == '\\' && s.escapes && strings.ContainsRune("\"\\$`\n", s.peek(1)):
			if s.peek(1) != '\n' {
				add(quoted, string(s.peek(1)))
			}
			s.i += 2
		case c == '$':
			s.dollar(w, add)
		case c == '`':
			s.i++
			w.parts = append(w.parts, s.substitution('`'))
		default:
			add(quoted, string(c))
			s.i++
		}
	}
}

// dollar reads an expansion at a $.
func (s *scanner) dollar(w *shellWord, add func(partKind, string)) {
	s.i++ // past the $
	switch c := s.peek(0); {
	case c == '(' && s.peek(1) == '(':
		s.i += 2
		depth := 2
		for s.i < len(s.rs) && depth > 0 {
			switch s.rs[s.i] {
			case '(':
				depth++
			case ')':
				depth--
			}
			s.i++
		}
		w.parts = append(w.parts, wordPart{kind: opaque})
	case c == '(':
		s.i++
		w.parts = append(w.parts, s.substitution(')'))
	case c == '{':
		s.i++
		end := slices.Index(s.rs[s.i:], '}')
		if end < 0 {
			end = len(s.rs) - s.i
		}
		w.parts = append(w.parts, braced(string(s.rs[s.i:s.i+end])))
		s.i = min(s.i+end+1, len(s.rs))
	case c == '\'':
		// $'...': quoted, its escapes taken as written.
		s.i++
		end := slices.Index(s.rs[s.i:], '\'')
		if end < 0 {
			end = len(s.rs) - s.i
		}
		add(quoted, string(s.rs[s.i:s.i+end]))
		s.i = min(s.i+end+1, len(s.rs))
	case c == '_' || unicode.IsLetter(c):
		start := s.i
		for s.i < len(s.rs) && (s.rs[s.i] == '_' || unicode.IsLetter(s.rs[s.i]) || unicode.IsDigit(s.rs[s.i])) {
			s.i++
		}
		w.parts = append(w.parts, wordPart{kind: variable, text: string(s.rs[start:s.i])})
	case c != 0 && (unicode.IsDigit(c) || strings.ContainsRune("@*#?$!-", c)):
		s.i++
		w.parts = append(w.parts, wordPart{kind: opaque})
	default:
		add(literal, "$")
	}
}

// substitution reads a command substitution up to closer, its words among the command's,
// and is the part it makes of the word that holds it: PWD for $(pwd), else opaque.
func (s *scanner) substitution(closer rune) wordPart {
	start, body := s.i, s.body
	s.body = false // a substitution runs, wherever it is
	s.commands(closer)
	s.body = body
	inner := strings.TrimSpace(strings.TrimSuffix(string(s.rs[start:s.i]), string(closer)))
	switch inner {
	case "pwd", "pwd -L", "pwd -P":
		return wordPart{kind: variable, text: "PWD"}
	}
	return wordPart{kind: opaque}
}

// braced is the part ${inner} makes: a parameter, with its fallback for ${NAME:-x} or
// ${NAME-x}; opaque for any other form.
func braced(inner string) wordPart {
	name, rest := inner, ""
	if i := strings.IndexFunc(inner, func(r rune) bool { return r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) }); i >= 0 {
		name, rest = inner[:i], inner[i:]
	}
	if !isName(name) {
		return wordPart{kind: opaque}
	}
	p := wordPart{kind: variable, text: name}
	switch {
	case rest == "":
	case strings.HasPrefix(rest, ":-"):
		f := rest[2:]
		p.fallback = &f
	case strings.HasPrefix(rest, "-"):
		f := rest[1:]
		p.fallback = &f
	default:
		return wordPart{kind: opaque}
	}
	return p
}

// isAssignment reports whether w is NAME=value.
func isAssignment(w shellWord) bool {
	if len(w.parts) == 0 || w.parts[0].kind != literal {
		return false
	}
	name, _, ok := strings.Cut(w.parts[0].text, "=")
	return ok && isName(name)
}
