// Package remediation reads the platform's active remediation campaign, an announcement
// that every terma older than a release, or not signed in as a person, must act, and
// judges whether it asks anything of this machine. Only `terma doctor` and `terma setup`
// ask: hooks, the relay and telemetry never do.
package remediation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

// Path is the auth gateway's public, credential-less route for the active campaign.
const Path = "/v1/remediation-campaigns/active?product=PRODUCT_TERMA_AI"

// Timeout bounds the whole fetch: a slow or absent platform costs a command at most this.
const Timeout = 3 * time.Second

// maxBody bounds what a fetch reads; a campaign is a few lines of text.
const maxBody = 64 << 10

// Campaign is the platform's active remediation campaign for terma.
type Campaign struct {
	ID                   string    `json:"id"`
	Title                string    `json:"title"`
	Message              string    `json:"message"`
	Steps                []string  `json:"steps"`
	MinimumClientVersion string    `json:"minimum_client_version"`
	RequireUserSession   bool      `json:"require_user_session"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// client takes no redirect: the route answers itself, and anything else is a failure.
var client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// Stub is the offline test override, TERMA_REMEDIATION_STUB: when set, Fetch decodes it as
// the route's answer and reaches nothing.
func Stub() string { return os.Getenv("TERMA_REMEDIATION_STUB") }

// Fetch asks the auth gateway at authURL for the active campaign, with no credential: it
// is nil when there is none. Any failure (offline, a timeout, an older platform without
// the route, a malformed answer) is an error the caller only logs.
func Fetch(ctx context.Context, authURL, version string) (*Campaign, error) {
	// The stub reaches nothing, so it needs no timeout.
	if stub := Stub(); stub != "" {
		return decode(strings.NewReader(stub))
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(authURL, "/")+Path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "terma-cli/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return nil, fmt.Errorf("remediation campaign: %s", resp.Status)
	}
	return decode(io.LimitReader(resp.Body, maxBody))
}

func decode(r io.Reader) (*Campaign, error) {
	var body struct {
		Campaign *Campaign `json:"campaign"`
	}
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return nil, fmt.Errorf("remediation campaign: %w", err)
	}
	c := body.Campaign
	if c == nil {
		return nil, nil
	}
	// The text comes from the network and goes to a terminal: nothing that is not printable,
	// neither control characters nor the format ones (bidi overrides, zero widths) that
	// would let it show an instruction other than the one it holds.
	c.Title, c.Message = printable(c.Title), printable(c.Message)
	steps := c.Steps[:0]
	for _, s := range c.Steps {
		if s = printable(s); s != "" {
			steps = append(steps, s)
		}
	}
	c.Steps = steps
	c.MinimumClientVersion = strings.TrimSpace(c.MinimumClientVersion)
	return c, nil
}

// printable is s on one line, each rune that is not printable a space.
func printable(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, s))
}

// Notice is what a campaign asks of this machine.
type Notice struct {
	Campaign Campaign
	// Local is this terma's version.
	Local string
	// Behind is a release older than the campaign's minimum; NeedsUserSession a campaign
	// that wants a person's sign-in on a machine without one.
	Behind, NeedsUserSession bool
	// KeyInEnv is a server key in TERMA_API_KEY, under which setup signs in with that key,
	// never as the person: it must be unset first.
	KeyInEnv bool
}

// Evaluate judges c for a terma at version local whose active sign-in is a person's user
// session (userSession), or not (a server key, or no sign-in). Source, dirty and snapshot
// builds are never behind (selfupdate.Newer).
func Evaluate(c *Campaign, local string, userSession bool) Notice {
	if c == nil {
		return Notice{}
	}
	return Notice{Campaign: *c, Local: local,
		Behind:           selfupdate.Newer(local, c.MinimumClientVersion),
		NeedsUserSession: c.RequireUserSession && !userSession}
}

// Warn reports whether the notice asks anything of this machine.
func (n Notice) Warn() bool { return n.Behind || n.NeedsUserSession }

// Summary is the one line saying what is required and the commands that do it.
func (n Notice) Summary() string {
	minimum := n.Campaign.MinimumClientVersion
	setup := "`terma setup`"
	if n.NeedsUserSession && n.KeyInEnv {
		setup = "`terma setup` with TERMA_API_KEY unset"
	}
	switch {
	case n.Behind && n.NeedsUserSession:
		return fmt.Sprintf("terma %s or later, signed in as you, is required; this is %s, not signed in as you — run `terma update`, then %s", minimum, n.Local, setup)
	case n.Behind:
		return fmt.Sprintf("terma %s or later is required, this is %s — run `terma update`, then %s", minimum, n.Local, setup)
	case n.NeedsUserSession:
		return "terma must be signed in as you, and this machine is not (a server key, or no sign-in) — run " + setup
	}
	return ""
}

// Lines are the campaign's title and message, then its steps, numbered.
func (n Notice) Lines() []string {
	var lines []string
	for _, l := range []string{n.Campaign.Title, n.Campaign.Message} {
		if l != "" {
			lines = append(lines, l)
		}
	}
	for i, s := range n.Campaign.Steps {
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, s))
	}
	return lines
}
