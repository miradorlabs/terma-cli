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

// grantFixture is testdata/grant_input.json, the CLI's own cases for what it reads.
type grantFixture struct {
	DelegatedGrant struct {
		Identity         string `json:"identity"`
		ChallengePattern string `json:"challengePattern"`
		Label            string `json:"label"`
		MaxLine          int    `json:"maxLine"`
		TimeoutSeconds   int    `json:"timeoutSeconds"`
		Cases            []grantCase
	}
}

type grantCase struct {
	Name              string            `json:"name"`
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
	raw, err := os.ReadFile("testdata/grant_input.json")
	if err != nil {
		t.Fatal(err)
	}
	var f grantFixture
	if err := json.Unmarshal(raw, &f.DelegatedGrant); err != nil {
		t.Fatal(err)
	}
	return f
}

// desktopGrantFixture is testdata/delegated_grant.json, the verbatim copy of terma-desktop's
// shared/fixtures/auth.json delegatedGrant, the lines both sides keep to.
type desktopGrantFixture struct {
	In1 struct {
		UserID string `json:"user_id"`
		OrgID  string `json:"org_id"`
		Line   string `json:"line"`
	} `json:"in1"`
	In2 struct {
		Code string `json:"code"`
		Line string `json:"line"`
	} `json:"in2"`
	ErrorCodes []string `json:"errorCodes"`
	// Cases are first stdout lines and what the desktop makes of them; nil, it fails the run.
	Cases []struct {
		Name    string          `json:"name"`
		Out     string          `json:"out"`
		Request json.RawMessage `json:"request"`
	} `json:"cases"`
	Runs []struct {
		Name     string `json:"name"`
		CLIError string `json:"cliError"`
		Exit     int    `json:"exit"`
	} `json:"runs"`
}

func loadDesktopGrantFixture(t *testing.T) desktopGrantFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/delegated_grant.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		DelegatedGrant desktopGrantFixture `json:"delegatedGrant"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f.DelegatedGrant
}

// The constants the CLI keeps are the fixtures'.
func TestGrantConstantsAreTheFixtures(t *testing.T) {
	g := loadGrantFixture(t).DelegatedGrant
	if g.MaxLine != maxGrantLine || time.Duration(g.TimeoutSeconds)*time.Second != GrantTimeout {
		t.Fatalf("bounds: fixture %d bytes %ds, CLI %d bytes %s", g.MaxLine, g.TimeoutSeconds, maxGrantLine, GrantTimeout)
	}
	codes := []string{GrantNoCode, GrantBadInput, GrantRejected, GrantIdentityMismatch}
	if desk := loadDesktopGrantFixture(t); strings.Join(desk.ErrorCodes, ",") != strings.Join(codes, ",") {
		t.Fatalf("error codes: desktop %v, CLI %v", desk.ErrorCodes, codes)
	}
	if !strings.HasSuffix(g.Label, DesktopLabelSuffix) || !strings.HasSuffix(DesktopLabel(), DesktopLabelSuffix) {
		t.Fatalf("label %q, suffix %q", g.Label, DesktopLabelSuffix)
	}
}

// The desktop's contract, from the CLI's side: it reads the desktop's two lines as they are
// written there, the code passed on byte for byte, and prints only lines the desktop's
// reader takes, the reuse exactly as pinned.
func TestGrantMeetsTheDesktopsContract(t *testing.T) {
	desk := loadDesktopGrantFixture(t)
	in := strings.NewReader(desk.In1.Line + "\n" + desk.In2.Line + "\n")
	id, err := ReadGrantIdentity(t.Context(), in, time.Second)
	if err != nil || id.UserID != desk.In1.UserID || id.OrgID != desk.In1.OrgID {
		t.Fatalf("in1 = %+v, %v", id, err)
	}
	var out bytes.Buffer
	exchanger := &recordingExchanger{}
	if _, err := LoginWithGrant(t.Context(), exchanger, GrantOptions{Label: "box" + DesktopLabelSuffix, In: in, Out: &out, Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	if exchanger.code != desk.In2.Code {
		t.Fatalf("exchanged %q, want in2's %q verbatim", exchanger.code, desk.In2.Code)
	}
	accepted := map[string]json.RawMessage{}
	for _, c := range desk.Cases {
		accepted[c.Name] = c.Request
	}
	// The request has the shape of the desktop's "a grant": challenge and label, nothing else.
	var want, got map[string]string
	if err := json.Unmarshal(accepted["a grant"], &want); err != nil {
		t.Fatalf("the desktop's \"a grant\" case: %v", err)
	}
	if err := json.Unmarshal(bytes.TrimSuffix(out.Bytes(), []byte("\n")), &got); err != nil || len(got) != len(want) ||
		!regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(got["challenge"]) || got["label"] != "box"+DesktopLabelSuffix ||
		!strings.HasSuffix(want["label"], DesktopLabelSuffix) {
		t.Fatalf("request %q, want the shape of %v", out.String(), want)
	}
	var reused bytes.Buffer
	if err := WriteGrantReused(&reused); err != nil {
		t.Fatal(err)
	}
	for _, c := range desk.Cases {
		if c.Name == "a stored sign-in for the same user and organization" && reused.String() != c.Out+"\n" {
			t.Fatalf("reuse line %q, want %q", reused.String(), c.Out)
		}
	}
	// The message the desktop's run shows for a missing code.
	for _, r := range desk.Runs {
		var line struct {
			Error struct{ Code, Message string }
		}
		if json.Unmarshal([]byte(r.CLIError), &line) != nil || line.Error.Code != GrantNoCode {
			continue
		}
		_, err := LoginWithGrant(t.Context(), exchanger, GrantOptions{In: strings.NewReader(""), Out: io.Discard, Timeout: time.Second})
		var ge *GrantError
		if !errors.As(err, &ge) || ge.Code != line.Error.Code || ge.Message != line.Error.Message {
			t.Fatalf("%s: err = %v, want %s %q", r.Name, err, line.Error.Code, line.Error.Message)
		}
	}
}

// Each case of the fixture, run through what `terma setup --desktop-grant` runs: the
// identity line, then the reuse or the grant.
func TestGrantProtocolFixture(t *testing.T) {
	g := loadGrantFixture(t).DelegatedGrant
	challenge := regexp.MustCompile(g.ChallengePattern)
	for _, tc := range g.Cases {
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

// Terma's ids are lowercase, so an identity spelled in uppercase is the same user and
// organization: compared with a stored sign-in and with the exchanged one, it still matches.
func TestGrantIdentityIsLowercase(t *testing.T) {
	line := `{"user_id":"0F8FAD5B-D9CB-469F-A165-70867728950E","org_id":"7C9E6679-7425-40DE-944B-E07FC1F90AE7"}` + "\n"
	id, err := ReadGrantIdentity(t.Context(), strings.NewReader(line), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if want := (GrantIdentity{UserID: "0f8fad5b-d9cb-469f-a165-70867728950e", OrgID: "7c9e6679-7425-40de-944b-e07fc1f90ae7"}); id != want {
		t.Fatalf("identity = %+v, want %+v", id, want)
	}
}

// runGrant is the CLI's side of one desktop run: the identity, then the reuse a stored
// sign-in allows, or the grant. Whether a stored sign-in may be reused (the same user and
// organization) is the CLI's check, tested in internal/cli; here stored only stands for it.
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
