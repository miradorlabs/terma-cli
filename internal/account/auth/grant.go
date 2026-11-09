package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// The desktop grant: Terma Desktop signs this CLI in with no browser and no listener.
// `terma setup --desktop-grant` reads the desktop's identity from stdin, and either reuses
// its own stored sign-in for that user and organization, or prints a PKCE challenge it
// made itself, reads back a code the desktop obtained for that challenge, and redeems it
// with its own verifier. The lines are pinned by terma-desktop's
// shared/fixtures/auth.json delegatedGrant, copied to testdata/delegated_grant.json:
//
//	stdin  1  {"user_id":"<uuid>","org_id":"<uuid>"}
//	stdout 1  {"challenge":"<S256>","label":"<label>"}  or  {"reused":true}
//	stdin  2  {"code":"<code>"}  (only after a challenge)
//
// The verifier never leaves this process and the code is never printed or logged.

// The error codes a failed grant prints on stdout, a contract with Terma Desktop.
const (
	// GrantNoCode: stdin ended, or stayed silent for GrantTimeout, before a code.
	GrantNoCode = "grant_no_code"
	// GrantBadInput: an identity or code line that is not the protocol's, or an
	// identity for another organization than --org.
	GrantBadInput = "grant_bad_input"
	// GrantRejected: the auth host did not redeem the code.
	GrantRejected = "grant_rejected"
	// GrantIdentityMismatch: the code signed in another user or organization than the
	// desktop's.
	GrantIdentityMismatch = "grant_identity_mismatch"
)

// GrantTimeout bounds the wait for each line from the desktop, so a stuck parent cannot
// hold terma open.
const GrantTimeout = 2 * time.Minute

// maxGrantLine bounds each line the desktop writes; a code and two ids need far less.
const maxGrantLine = 8 << 10

// DesktopLabelSuffix ends the session label of a sign-in Terma Desktop delegated, so the
// session list on the web, and `terma doctor`, tell it from a browser sign-in.
const DesktopLabelSuffix = " (via Terma Desktop)"

// DesktopLabel is the session label a desktop grant asks for.
func DesktopLabel() string { return DefaultLabel() + DesktopLabelSuffix }

// GrantError is a failed desktop grant: Code is what the desktop reads, Message what a
// person reads. Neither ever carries the code, a token or the verifier.
type GrantError struct {
	Code    string
	Message string
	// Err is the cause, for stderr.
	Err error
}

func (e *GrantError) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *GrantError) Unwrap() error { return e.Err }

// GrantIdentity is who the desktop is signed in as, the first line it writes.
type GrantIdentity struct {
	UserID string `json:"user_id"`
	OrgID  string `json:"org_id"`
}

var grantUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ReadGrantIdentity reads the desktop's identity line: two ids, no secret. timeout 0 is
// GrantTimeout.
func ReadGrantIdentity(ctx context.Context, in io.Reader, timeout time.Duration) (GrantIdentity, error) {
	line, err := readGrantLine(ctx, in, timeout)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return GrantIdentity{}, ctxErr
		}
		return GrantIdentity{}, &GrantError{Code: GrantBadInput, Message: "Terma Desktop sent no identity line on stdin", Err: err}
	}
	var id GrantIdentity
	if json.Unmarshal(line, &id) != nil || !grantUUID.MatchString(id.UserID) || !grantUUID.MatchString(id.OrgID) {
		return GrantIdentity{}, &GrantError{Code: GrantBadInput, Message: `the identity line on stdin is not {"user_id":"<uuid>","org_id":"<uuid>"}`}
	}
	return id, nil
}

// WriteGrantReused tells the desktop this CLI's own stored sign-in serves: no code is needed.
func WriteGrantReused(out io.Writer) error {
	return writeGrantLine(out, struct {
		Reused bool `json:"reused"`
	}{true})
}

// GrantOptions is what a desktop grant needs beyond the token exchange.
type GrantOptions struct {
	// Label names this session on the web; DesktopLabel.
	Label string
	// In is the desktop's pipe, past the identity line; Out is where the challenge goes,
	// as stdout's first line.
	In  io.Reader
	Out io.Writer
	// Timeout overrides GrantTimeout when nonzero.
	Timeout time.Duration
}

// LoginWithGrant signs in with a code Terma Desktop obtains for this process: it makes a
// PKCE pair, prints the challenge and the label as one JSON line, reads the code back as
// one, and redeems it with the verifier, which never leaves the process. No listener, no
// browser. Every failure is a *GrantError.
func LoginWithGrant(ctx context.Context, exchanger TokenExchanger, opts GrantOptions) (*Credential, error) {
	pkce, err := NewPKCE()
	if err != nil {
		return nil, err
	}
	request := struct {
		Challenge string `json:"challenge"`
		Label     string `json:"label"`
	}{pkce.Challenge, opts.Label}
	if err := writeGrantLine(opts.Out, request); err != nil {
		return nil, fmt.Errorf("write the grant request: %w", err)
	}
	line, err := readGrantLine(ctx, opts.In, opts.Timeout)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, errGrantLineTooLong):
		return nil, &GrantError{Code: GrantBadInput, Message: "the code line on stdin is too long"}
	default:
		return nil, &GrantError{Code: GrantNoCode, Message: "no code on stdin", Err: err}
	}
	var reply struct {
		Code *string `json:"code"`
	}
	// The JSON error is left out: it may quote what was sent.
	if json.Unmarshal(line, &reply) != nil || reply.Code == nil || strings.TrimSpace(*reply.Code) == "" {
		return nil, &GrantError{Code: GrantBadInput, Message: `the code line on stdin is not {"code":"…"}`}
	}
	cred, err := exchanger.ExchangeDelegatedCode(ctx, *reply.Code, pkce.Verifier)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &GrantError{Code: GrantRejected, Message: "Terma did not accept the code from Terma Desktop", Err: err}
	}
	return cred, nil
}

func writeGrantLine(out io.Writer, v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = out.Write(append(line, '\n'))
	return err
}

var (
	errGrantTimeout     = errors.New("timed out")
	errGrantLineTooLong = fmt.Errorf("a line over %d bytes", maxGrantLine)
)

// readGrantLine reads one line from in, without its newline. It reads no further than the
// newline, so a later read gets the next line, and it gives up after timeout (0:
// GrantTimeout) or when ctx ends; a read still blocked then is left to the process's exit.
// A last line without a newline counts; EOF before any byte is io.EOF.
func readGrantLine(ctx context.Context, in io.Reader, timeout time.Duration) ([]byte, error) {
	if timeout == 0 {
		timeout = GrantTimeout
	}
	type result struct {
		line []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := readLine(in)
		done <- result{line, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.line, r.err
	case <-timer.C:
		return nil, fmt.Errorf("%w after %s", errGrantTimeout, timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// readLine reads up to a newline a byte at a time: unbuffered, so nothing past it is taken
// from a pipe another read is to get.
func readLine(in io.Reader) ([]byte, error) {
	var line []byte
	var b [1]byte
	for {
		n, err := in.Read(b[:])
		if n > 0 {
			if b[0] == '\n' {
				return line, nil
			}
			if len(line) == maxGrantLine {
				return nil, errGrantLineTooLong
			}
			line = append(line, b[0])
		}
		switch {
		case errors.Is(err, io.EOF) && len(line) > 0:
			return line, nil
		case err != nil:
			return nil, err
		}
	}
}
