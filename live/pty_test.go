package live

import (
	"testing"
	"time"

	"github.com/hinshun/vt10x"
)

func TestPromptMatchingReconstructsCursorPosition(t *testing.T) {
	term := &Terminal{screen: vt10x.New(vt10x.WithSize(80, 24))}
	update := "banner\x1b[3;1H❯ "
	_, _ = term.screen.Write([]byte(update))
	term.raw.WriteString(update)
	if promptRE.MatchString(Strip(update)) {
		t.Fatal("fixture must require screen reconstruction")
	}
	if i, err := term.ExpectAny(time.Second, loginRE, promptRE); err != nil || i != 1 {
		t.Fatalf("visible prompt not detected: %d, %v", i, err)
	}
}

func TestReplyMatchingReconstructsIncrementalRedraw(t *testing.T) {
	term := &Terminal{screen: vt10x.New(vt10x.WithSize(80, 24))}
	// T is already on screen when the update inserts the reply around it.
	_, _ = term.screen.Write([]byte("\x1b[1;3HT"))
	term.Consume()
	update := "\x1b[1;1H⏺ \x1b[1;4HERMA_OK"
	_, _ = term.screen.Write([]byte(update))
	term.raw.WriteString(update)
	if termaOK.MatchString(Strip(update)) {
		t.Fatal("fixture must require screen reconstruction")
	}
	if term.matchSince(termaOK) == "" {
		t.Fatalf("reply lost during redraw: %q", term.screen.String())
	}
	term.Consume()
	if term.matchSince(termaOK) != "" {
		t.Fatal("previous reply satisfied a new turn")
	}
	update = "\x1b[3;1H⏺ TERMA_OK"
	_, _ = term.screen.Write([]byte(update))
	term.raw.WriteString(update)
	if term.matchSince(termaOK) == "" {
		t.Fatal("new reply not detected")
	}
}
