package cli

import (
	"bufio"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/ui/output"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

// pickRow is one line of a picker: a label and an optional note beside it.
type pickRow struct {
	Label string
	Note  string
	// Current marks the row already selected.
	Current bool
	// Default is the row a bare Enter selects; without one, Enter selects nothing.
	Default bool
}

// pick prompts for one of rows and returns its index: arrow keys on a terminal, else a
// numbered list answered by number or by name (match). Without a terminal it refuses
// rather than hang a pipeline on stdin.
func pick(cmd *cobra.Command, title string, rows []pickRow, match func(string) (int, error)) (int, error) {
	if canPrompt() {
		items := make([]prompt.Item, len(rows))
		for i, r := range rows {
			note := r.Note
			if r.Current {
				note = strings.TrimSpace(note + "  (current)")
			}
			items[i] = prompt.Item{Label: r.Label, Detail: note, Selected: r.Default || r.Current}
		}
		i, err := prompt.Choose(title, items)
		if errors.Is(err, prompt.ErrCancelled) {
			return -1, errCancelled
		}
		return i, err
	}
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
