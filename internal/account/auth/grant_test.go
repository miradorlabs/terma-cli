package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// grantFixture is testdata/delegated_grant.json, terma-desktop's delegatedGrant section.
type grantFixture struct {
	DelegatedGrant struct {
		Identity         string   `json:"identity"`
		ChallengePattern string   `json:"challengePattern"`
		Label            string   `json:"label"`
		MaxLine          int      `json:"maxLine"`
		TimeoutSeconds   int      `json:"timeoutSeconds"`
		ErrorCodes       []string `json:"errorCodes"`
		Cases            []grantCase
	} `json:"delegatedGrant"`
}

type grantCase struct {
	Name              string            `json:"name"`
	Side              string            `json:"side"`
	Stored            bool              `json:"stored"`
	Stdin             []string          `json:"stdin"`
	NoFinalNewline    bool              `json:"noFinalNewline"`
	HoldOpen          bool              `json:"holdOpen"`
	OversizedCodeLine bool              `json:"oversizedCodeLine"`
	ExchangeRefused   bool              `json:"exchangeRefused"`
	Stdout            []json.RawMessage `json:"stdout"`
	Exchanged         string            `json:"exchanged"`
	Error             string            `json:"error"`
}

func loadGrantFixture(t *testing.T) grantFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/delegated_grant.json")
	if err != nil {
		t.Fatal(err)
	}
	var f grantFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// The constants the CLI keeps are the fixture's.
func TestGrantConstantsAreTheFixtures(t *testing.T) {
	g := loadGrantFixture(t).DelegatedGrant
	if g.MaxLine != maxGrantLine || time.Duration(g.TimeoutSeconds)*time.Second != GrantTimeout {
		t.Fatalf("bounds: fixture %d bytes %ds, CLI %d bytes %s", g.MaxLine, g.TimeoutSeconds, maxGrantLine, GrantTimeout)
	}
	codes := []string{GrantNoCode, GrantBadInput, GrantRejected, GrantIdentityMismatch}
	if strings.Join(g.ErrorCodes, ",") != strings.Join(codes, ",") {
		t.Fatalf("error codes: fixture %v, CLI %v", g.ErrorCodes, codes)
	}
	if !strings.HasSuffix(g.Label, DesktopLabelSuffix) || !strings.HasSuffix(DesktopLabel(), DesktopLabelSuffix) {
		t.Fatalf("label %q, suffix %q", g.Label, DesktopLabelSuffix)
	}
}

// Each case of the fixture, run through what `terma setup --desktop-grant` runs: the
// identity line, then the reuse or the grant.
func TestGrantProtocolFixture(t *testing.T) {
	g := loadGrantFixture(t).DelegatedGrant
	challenge := regexp.MustCompile(g.ChallengePattern)
	for _, tc := range g.Cases {
		if tc.Side == "desktop" {
			continue // only the desktop's reader can fail it; TestGrantPrintsOneWellFormedLine covers the CLI's side
		}
		t.Run(tc.Name, func(t *testing.T) {
			in := desktopPipe(t, tc)
			var out bytes.Buffer
			exchanger := &recordingExchanger{}
			if tc.ExchangeRefused {
				exchanger.err = errors.New("invalid_grant: code already used")
			}
			err := runGrant(t, in, &out, exchanger, g.Label, tc.Stored)

			lines := outLines(out.String())
			if len(lines) != len(tc.Stdout) {
				t.Fatalf("stdout = %q, want %d lines", out.String(), len(tc.Stdout))
			}
			for i, want := range tc.Stdout {
				if string(want) == `"request"` {
					var req map[string]string
					if json.Unmarshal([]byte(lines[i]), &req) != nil || len(req) != 2 || !challenge.MatchString(req["challenge"]) || req["label"] != g.Label {
						t.Fatalf("stdout line %d = %s, want {challenge, label}", i+1, lines[i])
					}
					if exchanger.verifier != "" && s256(exchanger.verifier) != req["challenge"] {
						t.Fatalf("the code was redeemed with a verifier that is not the printed challenge's")
					}
				} else if !jsonEqual(t, lines[i], want) {
					t.Fatalf("stdout line %d = %s, want %s", i+1, lines[i], want)
				}
			}
			if tc.Error != "" {
				var ge *GrantError
				if !errors.As(err, &ge) || ge.Code != tc.Error {
					t.Fatalf("err = %v, want %s", err, tc.Error)
				}
				for _, line := range tc.Stdin {
					var sent struct{ Code string }
					if json.Unmarshal([]byte(line), &sent) == nil && sent.Code != "" && strings.Contains(err.Error(), sent.Code) {
						t.Fatalf("the error quotes the code: %v", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.Exchanged != "" && (exchanger.code != tc.Exchanged || !exchanger.delegated || exchanger.port != 0) {
				t.Fatalf("exchanged %q delegated=%v port %d, want %q delegated on port 0", exchanger.code, exchanger.delegated, exchanger.port, tc.Exchanged)
			}
			if exchanger.verifier != "" && strings.Contains(out.String(), exchanger.verifier) {
				t.Fatal("the verifier reached stdout")
			}
		})
	}
}

// What the desktop's reader fails on, the CLI never prints: its first line is one JSON
// object, the request, and nothing follows it before the code.
func TestGrantPrintsOneWellFormedLine(t *testing.T) {
	in, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	out := &lineWriter{lines: make(chan string, 4)}
	exchanger := &recordingExchanger{}
	done := make(chan error, 1)
	go func() {
		_, err := LoginWithGrant(t.Context(), exchanger, GrantOptions{Label: "box" + DesktopLabelSuffix, In: in, Out: out})
		done <- err
	}()
	first := <-out.lines
	var req struct{ Challenge, Label string }
	if err := json.Unmarshal([]byte(first), &req); err != nil || req.Challenge == "" {
		t.Fatalf("first line %q is not the request: %v", first, err)
	}
	select {
	case extra := <-out.lines:
		t.Fatalf("a second line before the code: %q", extra)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := io.WriteString(w, `{"code":"c1"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s256(exchanger.verifier) != req.Challenge {
		t.Fatal("the verifier redeemed is not the printed challenge's")
	}
}

// The identity line is read to its newline and no further, so the code line, written at
// once behind it as a desktop may, is still there for the grant.
func TestGrantIdentityLeavesTheCodeLine(t *testing.T) {
	in := strings.NewReader(loadGrantFixture(t).DelegatedGrant.Identity + "\n" + `{"code":"c1"}` + "\n")
	if _, err := ReadGrantIdentity(t.Context(), in, time.Second); err != nil {
		t.Fatal(err)
	}
	exchanger := &recordingExchanger{}
	if _, err := LoginWithGrant(t.Context(), exchanger, GrantOptions{In: in, Out: io.Discard, Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	if exchanger.code != "c1" {
		t.Fatalf("exchanged %q", exchanger.code)
	}
}

// runGrant is the CLI's side of one desktop run: the identity, then the reuse a stored
// sign-in allows, or the grant.
func runGrant(t *testing.T, in io.Reader, out io.Writer, exchanger TokenExchanger, label string, stored bool) error {
	t.Helper()
	ctx := t.Context()
	if _, err := ReadGrantIdentity(ctx, in, time.Second); err != nil {
		return err
	}
	if stored {
		return WriteGrantReused(out)
	}
	_, err := LoginWithGrant(ctx, exchanger, GrantOptions{Label: label, In: in, Out: out, Timeout: 100 * time.Millisecond})
	return err
}

// desktopPipe writes the case's stdin as the desktop would, through a pipe, and closes it
// unless the case holds it open.
func desktopPipe(t *testing.T, tc grantCase) io.Reader {
	t.Helper()
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close(); _ = r.Close() })
	var b strings.Builder
	for i, line := range tc.Stdin {
		b.WriteString(line)
		if i < len(tc.Stdin)-1 || !tc.NoFinalNewline {
			b.WriteByte('\n')
		}
	}
	if tc.OversizedCodeLine {
		b.WriteString(`{"code":"` + strings.Repeat("c", maxGrantLine) + `"}` + "\n")
	}
	go func() {
		_, _ = io.WriteString(w, b.String())
		if !tc.HoldOpen {
			_ = w.Close()
		}
	}()
	return r
}

func outLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func jsonEqual(t *testing.T, got string, want json.RawMessage) bool {
	t.Helper()
	var a, b any
	if json.Unmarshal([]byte(got), &a) != nil || json.Unmarshal(want, &b) != nil {
		return false
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// lineWriter hands each line written to it to a channel.
type lineWriter struct{ lines chan string }

func (w *lineWriter) Write(p []byte) (int, error) {
	for line := range strings.SplitSeq(strings.TrimSuffix(string(p), "\n"), "\n") {
		w.lines <- line
	}
	return len(p), nil
}

// A context that ends while the desktop is silent ends the wait, not as a missing code.
func TestGrantStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	identity := loadGrantFixture(t).DelegatedGrant.Identity
	go func() { _, _ = io.WriteString(w, identity+"\n") }()
	if _, err := ReadGrantIdentity(ctx, r, time.Second); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := LoginWithGrant(ctx, &recordingExchanger{}, GrantOptions{In: r, Out: io.Discard})
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
