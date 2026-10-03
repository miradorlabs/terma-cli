package cli

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/miradorlabs/terma-cli/internal/ui/output"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

// errCancelled is printed as "Cancelled" and returned as nil, like a declined confirm.
var errCancelled = errors.New("cancelled")

// canPrompt needs both halves: output.Interactive covers stdout and agent detection,
// prompt.Interactive stdin, so piped input falls through to the line-based confirm.
func canPrompt() bool {
	return output.Interactive() && prompt.Interactive()
}

func confirm(cmd *cobra.Command, question string) (bool, error) {
	return confirmDefault(cmd, question, true)
}

func confirmDefault(cmd *cobra.Command, question string, def bool) (bool, error) {
	return confirmExplained(cmd, question, nil, def)
}

func confirmExplained(cmd *cobra.Command, question string, detail []string, def bool) (bool, error) {
	in := cmd.InOrStdin()
	interactive := false
	// /dev/null (including go test's stdin) is not an explicit piped answer.
	if f, ok := in.(*os.File); ok && !term.IsTerminal(int(f.Fd())) {
		if info, err := f.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return false, fmt.Errorf("%s — no input to confirm with; pass --yes or pipe an answer", question)
		}
	}
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		if !canPrompt() {
			return false, fmt.Errorf("%s — no terminal to confirm on; pass --yes to proceed non-interactively", question)
		}
		state, err := term.MakeRaw(int(f.Fd()))
		if err != nil {
			return false, fmt.Errorf("read confirmation: %w", err)
		}
		defer func() { _ = term.Restore(int(f.Fd()), state) }()
		interactive = true
	}
	errOut := cmd.ErrOrStderr()
	text := confirmPrompt(style.For(errOut), question, detail, def)
	if interactive {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	fmt.Fprint(errOut, text)
	answer, err := readConfirmation(in, interactive, def)
	if interactive {
		if err == nil {
			if answer {
				fmt.Fprint(errOut, "y")
			} else {
				fmt.Fprint(errOut, "n")
			}
		}
		fmt.Fprint(errOut, "\r\n")
	}
	return answer, err
}

func confirmPrompt(p style.Palette, question string, detail []string, def bool) string {
	hint := "[Y/n]"
	if !def {
		hint = "[y/N]"
	}
	if len(detail) == 0 {
		return fmt.Sprintf("%s %s %s ", p.Brand("?"), p.Bold(question), p.Dim(hint))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", p.Brand("?"), p.Bold(question))
	for _, l := range detail {
		fmt.Fprintf(&b, "  %s\n", l)
	}
	fmt.Fprintf(&b, "  %s ", p.Dim(hint))
	return b.String()
}

// yesAnswer treats anything but y, yes or nothing as no, so a typo never agrees to something.
func yesAnswer(line string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "":
		return def
	}
	return false
}

func joinNames(names []string) string {
	return cmp.Or(output.And(names), "your coding agents")
}
