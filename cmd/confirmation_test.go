package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestConfirmationSingleKey(t *testing.T) {
	for _, tc := range []struct {
		input     string
		def, want bool
		cancel    bool
	}{
		{"y", false, true, false}, {"Y", false, true, false},
		{"n", true, false, false}, {"N", true, false, false},
		{"\r", true, true, false}, {"\n", false, false, false},
		{"xy", false, true, false}, {"\x03", true, false, true},
		{"\x1b", true, false, true}, {"", true, false, false},
	} {
		got, err := readConfirmation(strings.NewReader(tc.input), true, tc.def)
		if got != tc.want || (tc.cancel && !errors.Is(err, errCancelled)) || (!tc.cancel && err != nil) {
			t.Errorf("input %q: got %v, %v", tc.input, got, err)
		}
	}
}

func TestConfirmationsSharePipedAnswers(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader("yes\nn\n\ny"))
	cmd.SetErr(io.Discard)
	for i, want := range []bool{true, false, true, true, false} {
		got, err := confirmDefault(cmd, "Continue?", true)
		if err != nil || got != want {
			t.Fatalf("prompt %d: got %v, %v; want %v", i, got, err, want)
		}
	}
}

func TestBrowserEnterRequiresNewline(t *testing.T) {
	for _, input := range []string{"\n", "", "y"} {
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		cmd.SetIn(strings.NewReader(input))
		var out bytes.Buffer
		cmd.SetErr(&out)
		err := waitForBrowserEnter(cmd)
		if (err == nil) != (input == "\n") {
			t.Errorf("input %q: %v", input, err)
		}
		if !strings.Contains(out.String(), "Press Enter") {
			t.Fatal("missing browser handoff explanation")
		}
	}
}
