package e2e

import (
	"bufio"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// A build's command line: its --help and the help of every subcommand it lists, one level deep,
// each recorded as RecordHelp records it.

var (
	// helpCommandsRE finds the header of a help text's list of subcommands.
	helpCommandsRE = regexp.MustCompile(`(?i)^\s*(commands|subcommands|available commands)\s*:?\s*$`)
	// argparseCommandsRE finds argparse's list of subcommands: "{chat,model,…}".
	argparseCommandsRE = regexp.MustCompile(`^\s*\{([A-Za-z0-9_,-]+)\}`)
)

// HelpCommands are the subcommands a help text lists: a "Commands:" section's (Commander's,
// clap's, yargs' and cobra's), else argparse's "{a,b,c}"; help itself left out.
func HelpCommands(help, program string) []string {
	var out []string
	add := func(name string) {
		if name != "" && name != "help" && !strings.ContainsAny(name[:1], "-<[*{") && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	sc := bufio.NewScanner(strings.NewReader(help))
	in, indent := false, -1
	var argparse []string
	for sc.Scan() {
		line := sc.Text()
		if m := argparseCommandsRE.FindStringSubmatch(line); m != nil && argparse == nil {
			argparse = strings.Split(m[1], ",")
		}
		if helpCommandsRE.MatchString(line) {
			in, indent = true, -1
			continue
		}
		if !in || strings.TrimSpace(line) == "" {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			in = false
			continue
		}
		// An entry starts at the section's indent; deeper lines continue a description.
		at := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent < 0 {
			indent = at
		}
		if at != indent {
			continue
		}
		fields := strings.Fields(line)
		name := fields[0]
		if (name == program || name == filepath.Base(program)) && len(fields) > 1 {
			name = fields[1]
		}
		name, _, _ = strings.Cut(strings.TrimSuffix(name, ","), "|")
		add(name)
	}
	if len(out) == 0 {
		for _, name := range argparse {
			add(name)
		}
	}
	return out
}

// helpWorkers is how many subcommands' help run at once.
const helpWorkers = 6

// RecordHelpTree records b's --help, and the help of each subcommand it lists (HelpCommands),
// one level deep, in env; program is the name the build's help calls itself. Each one's -h is
// kept too where it prints something else (clap's -h is a summary of --help).
func RecordHelpTree(t *testing.T, b Binary, env []string, program string) {
	t.Helper()
	recordHelpBoth(t, b, env)
	p := profileOf(t, b)
	profilesMu.Lock()
	top := p.Help["--help"]
	profilesMu.Unlock()
	subs := make(chan string)
	var wg sync.WaitGroup
	for range helpWorkers {
		wg.Go(func() {
			for sub := range subs {
				recordHelpBoth(t, b, env, sub)
			}
		})
	}
	for _, sub := range HelpCommands(top, program) {
		subs <- sub
	}
	close(subs)
	wg.Wait()
}

// recordHelpBoth records the --help of b's command cmd (none: the build's own), and its -h
// where that prints something else.
func recordHelpBoth(t *testing.T, b Binary, env []string, cmd ...string) {
	t.Helper()
	long := strings.Join(append(append([]string{}, cmd...), "--help"), " ")
	short := strings.Join(append(append([]string{}, cmd...), "-h"), " ")
	RecordHelp(t, b, env, append(append([]string{}, cmd...), "--help")...)
	RecordHelp(t, b, env, append(append([]string{}, cmd...), "-h")...)
	p := profileOf(t, b)
	profilesMu.Lock()
	defer profilesMu.Unlock()
	if p.Help[short] == p.Help[long] {
		delete(p.Help, short)
	}
}
