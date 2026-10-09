package codex

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// wordsOf is a command's words, from the command it runs: past the wrappers that run the
// next word as a command (command, env and exec, with their options and env's assignments).
func wordsOf(call *syntax.CallExpr) []word {
	words := make([]word, len(call.Args))
	for i, a := range call.Args {
		words[i] = literal(a)
	}
	for len(words) > 0 {
		switch filepath.Base(words[0].text) {
		case "command", "exec":
			wrapper := filepath.Base(words[0].text)
			words = words[1:]
			for len(words) > 0 && strings.HasPrefix(words[0].text, "-") {
				option := words[0].text
				if option == "--" {
					words = words[1:]
					break
				}
				if wrapper == "command" && strings.ContainsAny(option, "vV") {
					return nil // command -v and -V look a command up; nothing runs
				}
				words = words[1:]
				if _, name, hasName := strings.Cut(strings.TrimPrefix(option, "-"), "a"); wrapper == "exec" && hasName {
					if name == "" && len(words) > 0 {
						words = words[1:] // exec -a NAME, including clustered -cla NAME
					}
					// Any attached suffix is the name; remaining words can still be options.
				}
			}
		case "env":
			words = words[1:]
			for len(words) > 0 && (strings.HasPrefix(words[0].text, "-") || strings.Contains(words[0].text, "=")) {
				switch a := words[0].text; {
				case a == "-S" || a == "--split-string":
					if len(words) < 2 || !words[1].literal {
						return nil
					}
					parts := splitEnvWords(words[1].text)
					if len(parts) == 0 {
						return nil
					}
					words = append(parts, words[2:]...)
					continue
				case strings.HasPrefix(a, "--split-string=") || strings.HasPrefix(a, "-S"):
					value := strings.TrimPrefix(strings.TrimPrefix(a, "--split-string="), "-S")
					parts := splitEnvWords(value)
					if !words[0].literal || len(parts) == 0 {
						return nil
					}
					words = append(parts, words[1:]...)
					continue
				case a == "-C" || a == "--chdir" || strings.HasPrefix(a, "--chdir="):
					return nil // run somewhere else, so its relative paths are unknown
				case a == "-u":
					words = words[1:] // the name to unset
				}
				words = words[1:]
			}
		default:
			return words
		}
	}
	return words
}

// splitEnvWords supports env -S's literal, quoted argument subset. Environment
// expansions and env-specific escapes are left unknown instead of interpreted as sh.
func splitEnvWords(value string) []word {
	if strings.ContainsAny(value, "\\$`") {
		return nil
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(value), "")
	if err != nil || len(file.Stmts) != 1 {
		return nil
	}
	stmt := file.Stmts[0]
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) != 0 || len(stmt.Redirs) != 0 || stmt.Background || stmt.Coprocess || stmt.Negated {
		return nil
	}
	var out []word
	for _, argument := range call.Args {
		w := literal(argument)
		if !w.literal {
			return nil
		}
		out = append(out, w)
	}
	return out
}
