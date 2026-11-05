package main

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/miradorlabs/terma-cli/e2e"
)

// The command line, itemized. A build's help texts (each command's --help, and -h where it
// says something else) are read into its commands: each command's options, by their long name,
// with their other names, the value they take, their description, default, allowed values and
// the environment variable that sets them; its arguments; and the commands it lists, with their
// descriptions. The layouts the harnesses print are a few (Commander's two columns, clap's long
// and short forms, yargs', argparse's), and read the same way: a heading ("Options:"), then
// entries whose description sits beside them or on the lines below, further in. A description is
// read as one line however the terminal wrapped it, so a narrower terminal is no change.

// cliOption is one option of one command.
type cliOption struct {
	Names       []string `json:"names"`
	Value       string   `json:"value,omitempty"`
	Description string   `json:"description,omitempty"`
	Default     string   `json:"default,omitempty"`
	Choices     []string `json:"choices,omitempty"`
	Env         string   `json:"env,omitempty"`
}

// cliCommand is one command: the build's own ("claude"), or one it lists ("codex exec").
type cliCommand struct {
	Usage       []string              `json:"usage,omitempty"`
	Description string                `json:"description,omitempty"`
	Options     map[string]*cliOption `json:"options,omitempty"`
	Arguments   map[string]*cliOption `json:"arguments,omitempty"`
	// Helped marks a command whose own help was read; one only listed has its description.
	Helped bool `json:"helped"`
}

var (
	headingRe = regexp.MustCompile(`^([A-Za-z][A-Za-z ]*):\s*$`)
	usageRe   = regexp.MustCompile(`(?i)^\s*usage:\s*(\S+)`)
	// Notes an option's description ends with: clap's [default: x] [possible values: a, b]
	// [env: X=], Commander's (choices: "a", "b", default: "x"), yargs' [default: x] [choices: …].
	bracketRe = regexp.MustCompile(`\[(default|possible values|choices|env|aliases|alias)s?:\s*([^\]]*)\]`)
	parenRe   = regexp.MustCompile(`\((choices|default|env):\s*([^()]*)\)`)
	spaces    = regexp.MustCompile(`\s+`)
)

// helpSection is what a heading in a help text lists.
func helpSection(heading string) string {
	h := strings.ToLower(heading)
	switch {
	case strings.Contains(h, "option") || strings.Contains(h, "flag"):
		return "options"
	case strings.Contains(h, "command"):
		return "commands"
	case strings.Contains(h, "argument") || strings.Contains(h, "positional"):
		return "arguments"
	}
	return ""
}

func indentOf(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

// helpEntry is an entry of a help section: its head, and its description's lines.
type helpEntry struct {
	head string
	desc []string
}

// parseHelp reads a help text into its usage lines, description and sections' entries.
func parseHelp(text string) (usage []string, description string, sections map[string][]helpEntry) {
	sections = map[string][]helpEntry{}
	lines := strings.Split(strings.ReplaceAll(text, "\t", "    "), "\n")
	section, entryIndent := "", -1
	var cur *helpEntry
	var desc []string
	flush := func() {
		if cur != nil && section != "" {
			sections[section] = append(sections[section], *cur)
		}
		cur = nil
	}
	inUsage := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if m := usageRe.FindStringSubmatch(line); m != nil && indentOf(line) < 2 {
			flush()
			section, inUsage = "", true
			usage = append(usage, spaces.ReplaceAllString(trimmed, " "))
			continue
		}
		if indentOf(line) == 0 && headingRe.MatchString(line) {
			flush()
			inUsage = false
			section, entryIndent = helpSection(headingRe.FindStringSubmatch(line)[1]), -1
			continue
		}
		if trimmed == "" {
			if cur != nil {
				cur.desc = append(cur.desc, "")
			}
			inUsage = false
			continue
		}
		if inUsage && indentOf(line) > 0 {
			usage = append(usage, spaces.ReplaceAllString(trimmed, " "))
			continue
		}
		if section == "" {
			if indentOf(line) == 0 && len(usage) > 0 || len(usage) == 0 {
				desc = append(desc, trimmed)
			}
			continue
		}
		ind := indentOf(line)
		if entryIndent < 0 {
			entryIndent = ind
		}
		// A new entry starts at the section's entry indent (an option's a little further in,
		// where clap aligns long names past short ones); anything deeper is description.
		starts := ind <= entryIndent+4 && (section != "options" || strings.HasPrefix(trimmed, "-"))
		if starts && ind < entryIndent {
			entryIndent = ind
		}
		if starts {
			flush()
			head, rest := splitColumns(trimmed)
			cur = &helpEntry{head: head}
			if rest != "" {
				cur.desc = append(cur.desc, rest)
			}
			continue
		}
		if cur != nil {
			cur.desc = append(cur.desc, trimmed)
		}
	}
	flush()
	return usage, strings.TrimSpace(strings.Join(desc, " ")), sections
}

// splitColumns splits an entry's line at the first gap of two spaces or more: its head, and the
// description beside it.
func splitColumns(line string) (head, rest string) {
	if i := strings.Index(line, "  "); i > 0 {
		return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i:])
	}
	return line, ""
}

// joinDescription is a description's lines as one text: a paragraph a line, wrapping undone.
func joinDescription(lines []string) string {
	var paras []string
	var p []string
	for _, l := range append(lines, "") {
		if l == "" {
			if len(p) > 0 {
				paras = append(paras, strings.Join(p, " "))
				p = nil
			}
			continue
		}
		p = append(p, l)
	}
	return strings.TrimSpace(strings.Join(paras, "\n"))
}

// parseOption reads an option's head ("-c, --config <key=value>", "--allowedTools,
// --allowed-tools <tools...>", "-m MODEL, --model MODEL") and description.
func parseOption(e helpEntry) *cliOption {
	o := &cliOption{}
	for part := range strings.SplitSeq(e.head, ",") {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) == 0 {
			continue
		}
		name, value, _ := strings.Cut(fields[0], "=")
		if strings.HasPrefix(name, "-") {
			o.Names = append(o.Names, name)
		}
		if value != "" {
			o.Value = value
		}
		if len(fields) > 1 {
			o.Value = strings.Join(fields[1:], " ")
		}
	}
	o.Description = noteOff(joinDescription(e.desc), o)
	return o
}

// noteOff takes the notes an option's description ends with (its default, choices and
// environment variable) into o, and is the description without them.
func noteOff(desc string, o *cliOption) string {
	take := func(kind, value string) {
		value = strings.TrimSpace(value)
		switch strings.ToLower(kind) {
		case "default":
			o.Default = strings.Trim(value, `"'`)
		case "possible values", "choices":
			for v := range strings.SplitSeq(value, ",") {
				if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" {
					o.Choices = append(o.Choices, v)
				}
			}
		case "env":
			o.Env = strings.TrimSuffix(strings.Fields(value + " ")[0], "=")
		}
	}
	for _, m := range bracketRe.FindAllStringSubmatch(desc, -1) {
		take(m[1], m[2])
	}
	desc = bracketRe.ReplaceAllString(desc, "")
	for _, m := range parenRe.FindAllStringSubmatch(desc, -1) {
		// Commander's "(choices: "a", "b", default: "x")" holds both.
		body := m[2]
		if kind := strings.ToLower(m[1]); kind == "choices" {
			if i := strings.Index(body, "default:"); i >= 0 {
				take("default", body[i+len("default:"):])
				body = strings.TrimSuffix(strings.TrimSpace(body[:i]), ",")
			}
		}
		take(m[1], body)
	}
	desc = parenRe.ReplaceAllString(desc, "")
	// Spaces left where a note was taken out go; the paragraphs stay.
	paras := strings.Split(desc, "\n")
	for i, p := range paras {
		paras[i] = strings.TrimSpace(spaces.ReplaceAllString(p, " "))
	}
	var out []string
	for _, p := range paras {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
}

// optionKey is what an option is known by: its first long name, or its first.
func optionKey(o *cliOption) string {
	for _, n := range o.Names {
		if strings.HasPrefix(n, "--") {
			return n
		}
	}
	if len(o.Names) > 0 {
		return o.Names[0]
	}
	return ""
}

// commandName is a listed command's name: "mcp [options]" → mcp, "add|a <x>" → add, its first
// word without the alias.
func commandName(head string) string {
	f := strings.Fields(head)
	if len(f) == 0 {
		return ""
	}
	name, _, _ := strings.Cut(strings.TrimSuffix(f[0], ","), "|")
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "[") || strings.HasPrefix(name, "<") {
		return ""
	}
	return name
}

// itemizeCLI reads a build's help texts into its commands, keyed by how a person types them
// ("codex", "codex exec").
func itemizeCLI(program string, help map[string]string) map[string]*cliCommand {
	commands := map[string]*cliCommand{}
	get := func(name string) *cliCommand {
		if commands[name] == nil {
			commands[name] = &cliCommand{}
		}
		return commands[name]
	}
	// Each command's own help: --help where there is one, its -h where there is not.
	byCommand := map[string]string{}
	for args, text := range help {
		if strings.Contains(text, "[fork/exec") || strings.Contains(text, "no such file or directory]") {
			continue // the build was not there to ask: nothing observed
		}
		cmd, long := strings.CutSuffix(args, " --help")
		if args == "--help" {
			cmd, long = "", true
		}
		if !long {
			c, short := strings.CutSuffix(args, " -h")
			if args == "-h" {
				c, short = "", true
			}
			if !short {
				continue
			}
			if _, has := byCommand[c]; has {
				continue
			}
			cmd = c
		}
		byCommand[cmd] = text
	}
	for cmd, text := range byCommand {
		name := strings.TrimSpace(program + " " + cmd)
		c := get(name)
		c.Helped = true
		var sections map[string][]helpEntry
		c.Usage, c.Description, sections = parseHelp(text)
		for _, e := range sections["options"] {
			if o := parseOption(e); optionKey(o) != "" {
				if c.Options == nil {
					c.Options = map[string]*cliOption{}
				}
				c.Options[optionKey(o)] = o
			}
		}
		for _, e := range sections["arguments"] {
			arg := strings.Trim(strings.Fields(e.head + " ")[0], "[]<>.")
			if arg == "" {
				continue
			}
			if c.Arguments == nil {
				c.Arguments = map[string]*cliOption{}
			}
			a := &cliOption{}
			a.Description = noteOff(joinDescription(e.desc), a)
			c.Arguments[arg] = a
		}
		for _, e := range sections["commands"] {
			sub := commandName(e.head)
			if sub == "" || sub == "help" {
				continue
			}
			s := get(name + " " + sub)
			if d := noteOff(joinDescription(e.desc), &cliOption{}); d != "" && (!s.Helped || s.Description == "") {
				s.Description = d
			}
		}
	}
	return commands
}

// programOf is what a build's help calls it, from its usage line ("Usage: claude [options]");
// harness where it says nothing.
func programOf(harness string, help map[string]string) string {
	if m := usageRe.FindStringSubmatch(help["--help"]); m != nil {
		return strings.TrimSuffix(m[1], ":")
	}
	if m := usageRe.FindStringSubmatch(help["-h"]); m != nil {
		return m[1]
	}
	return harness
}

// deriveCLI is the build's help texts, and its commands itemized from them.
func deriveCLI(s *Snapshot, p e2e.Profile) {
	if len(p.Help) == 0 {
		cliProbes(s, p)
		return
	}
	help := map[string]any{}
	keys := make([]string, 0, len(p.Help))
	for k := range p.Help {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		help[k] = p.Help[k]
	}
	commands := map[string]any{}
	for name, c := range itemizeCLI(programOf(p.Harness, p.Help), p.Help) {
		commands[name] = jsonOf(c)
	}
	s.CLI = map[string]any{"help": help, "commands": commands}
	cliProbes(s, p)
}

// cliProbes are what a desktop app's profile says of how it runs its agent: the agent's command
// line and environment as the app started it (cli.launch), and the JSON-RPC methods of an
// app-server the app drives (cli.app_server).
func cliProbes(s *Snapshot, p e2e.Profile) {
	for probe, key := range map[string]string{"launch": "launch", "app-server": "app_server"} {
		raw, ok := p.Probes[probe]
		if !ok {
			continue
		}
		var v any
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		if s.CLI == nil {
			s.CLI = map[string]any{}
		}
		s.CLI[key] = v
	}
}
