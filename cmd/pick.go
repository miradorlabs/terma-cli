package cmd

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/output"
	"github.com/miradorlabs/terma-cli/internal/style"
)

// pickRow is one line of a numbered picker: what it is called, and anything worth
// saying beside it (an id, "signed in", "current").
type pickRow struct {
	Label string
	Note  string
	// Current marks the row that is already selected, so the list says so.
	Current bool
	// Default is the row a bare Enter selects. Without one, Enter selects nothing.
	Default bool
}

// pick prompts for one of rows by number or by name and returns its index. It refuses
// to run without a terminal: a script that reaches this path wanted an argument, and
// blocking on stdin would hang a pipeline instead of failing it. match resolves a typed
// name the same way the command's argument would, so the picker and the argument
// agree on what a string means.
func pick(cmd *cobra.Command, title string, rows []pickRow, match func(string) (int, error)) (int, error) {
	if !output.Interactive() {
		return -1, fmt.Errorf("no selection given and no terminal to prompt on — pass a name or id")
	}

	out := cmd.ErrOrStderr()
	p := style.For(out)
	fmt.Fprintln(out, p.Bold(title))
	width, def := 0, -1
	for i, r := range rows {
		if n := len([]rune(r.Label)); n > width {
			width = n
		}
		if r.Default {
			def = i
		}
	}
	for i, r := range rows {
		number := p.Brand(fmt.Sprintf("%2d)", i+1))
		label := r.Label + strings.Repeat(" ", width-len([]rune(r.Label)))
		note := r.Note
		if r.Current {
			note = strings.TrimSpace(note + "  (current)")
		}
		fmt.Fprintf(out, "  %s %s  %s\n", number, label, p.Dim(note))
	}
	ask := "Number or name: "
	if def >= 0 {
		ask = fmt.Sprintf("Number or name (Enter for %s): ", rows[def].Label)
	}
	fmt.Fprint(out, "\n"+p.Brand("?")+" "+ask)

	reader := bufio.NewReader(cmd.InOrStdin())
	line, err := reader.ReadString('\n')
	if err != nil {
		return -1, fmt.Errorf("read selection: %w", err)
	}
	return pickAnswer(strings.TrimSpace(line), len(rows), def, match)
}

// pickAnswer reads what was typed at a picker of n rows: nothing takes the default row
// (def, or -1 for none), a number is a row, and anything else is a name for match.
func pickAnswer(answer string, n, def int, match func(string) (int, error)) (int, error) {
	if answer == "" {
		if def >= 0 {
			return def, nil
		}
		return -1, fmt.Errorf("no selection made")
	}
	if i, convErr := strconv.Atoi(answer); convErr == nil {
		if i < 1 || i > n {
			return -1, fmt.Errorf("selection %d is out of range (1-%d)", i, n)
		}
		return i - 1, nil
	}
	return match(answer)
}
