package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
)

func (app *App) newAuthCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "auth",
		Short:  "Credentials for programs that act as the signed-in developer",
		Hidden: true,
	}
	cmd.AddCommand(app.newAuthTokenCommand())
	return cmd
}

// The error codes `terma auth token` reports, a contract with the terma-mcp bridge.
const (
	tokenSignedOut   = "signed_out"
	tokenNoTeam      = "no_team"
	tokenRevoked     = "revoked"
	tokenUnavailable = "unavailable"
)

// tokenGrant is everything `terma auth token` prints on success: an allowlist, built field
// by field, so the refresh token can never reach it.
type tokenGrant struct {
	AccessToken    string      `json:"access_token"`
	ExpiresAt      string      `json:"expires_at"`
	OrganizationID string      `json:"organization_id"`
	TeamID         string      `json:"team_id"`
	Teams          []tokenTeam `json:"teams"`
	MCPURL         string      `json:"mcp_url"`
}

type tokenTeam struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// tokenFailure is what it prints instead, under "error"; its message names the fix.
type tokenFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// cause goes to stderr, never stdout.
	cause error
}

var sha256Hex = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

func (app *App) newAuthTokenCommand() *cobra.Command {
	var asJSON, forceRefresh bool
	var refusedSHA256 string
	cmd := &cobra.Command{
		Use:   "token --json",
		Short: "Print a fresh access token, the organization and the team, for the terma-mcp bridge",
		Long: `Prints one JSON object on stdout: a fresh access token with its expiry, the
organization, the team chosen at setup, the organization's teams and the MCP gateway's
URL. Never the refresh token. On failure it exits non-zero and prints
{"error": {"code": ..., "message": ...}}, the code one of signed_out, no_team,
revoked or unavailable.

--force-refresh rotates a token that has not expired yet, once a server refused it, but
only while the stored token is still the one refused, named by its SHA-256 in
--refused-token-sha256, so the token itself never appears on a command line.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !asJSON {
				return errors.New("only --json output is supported")
			}
			if forceRefresh != (refusedSHA256 != "") {
				return errors.New("--force-refresh and --refused-token-sha256 go together")
			}
			if refusedSHA256 != "" && !sha256Hex.MatchString(refusedSHA256) {
				return errors.New("--refused-token-sha256 takes the token's SHA-256 as 64 hex digits")
			}
			grant, failure := app.authToken(cmd.Context(), refusedSHA256)
			if failure != nil {
				return writeTokenFailure(cmd.OutOrStdout(), cmd.ErrOrStderr(), failure)
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(grant)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON (the only format)")
	cmd.Flags().BoolVar(&forceRefresh, "force-refresh", false, "rotate the access token although it has not expired")
	cmd.Flags().StringVar(&refusedSHA256, "refused-token-sha256", "", "with --force-refresh: hex SHA-256 of the access token a server refused")
	return cmd
}

func writeTokenFailure(stdout, stderr io.Writer, failure *tokenFailure) error {
	if failure.cause != nil {
		fmt.Fprintf(stderr, "terma auth token: %s: %v\n", failure.Code, failure.cause)
	}
	if err := json.NewEncoder(stdout).Encode(map[string]*tokenFailure{"error": failure}); err != nil {
		return err
	}
	return exitWith(1)
}

// authToken returns the signed-in developer's fresh access token and team, refreshed
// first when it is about to expire, or when refusedSHA256 names the stored one.
func (app *App) authToken(ctx context.Context, refusedSHA256 string) (*tokenGrant, *tokenFailure) {
	cfg, err := app.loadConfig()
	if err != nil {
		return nil, &tokenFailure{Code: tokenUnavailable, Message: "terma could not read its configuration: run `terma doctor`", cause: err}
	}
	switch {
	case cfg.APIKey != "":
		return nil, &tokenFailure{Code: tokenSignedOut,
			Message: "TERMA_API_KEY is set, and a server key cannot be used for the MCP: unset it and run `terma setup` to sign in as yourself"}
	case cfg.ServerKeySignIn:
		return nil, &tokenFailure{Code: tokenSignedOut,
			Message: "terma is signed in with a team's server key, which cannot be used for the MCP: run `terma setup` to sign in as yourself"}
	}
	client, err := api.New(cfg, api.Options{Version: app.version})
	if failure := tokenFailureOf(err, tokenSignedOut); failure != nil {
		return nil, failure
	}
	teamID := cmp.Or(cfg.ProjectID, cfg.Team)
	if teamID == "" {
		return nil, &tokenFailure{Code: tokenNoTeam, Message: "no Terma team is chosen: run `terma setup` to choose one"}
	}

	if refusedSHA256 != "" {
		_, err = client.RefreshRefused(ctx, refusedSHA256)
	} else {
		_, err = client.Fresh(ctx)
	}
	if failure := tokenFailureOf(err, tokenRevoked); failure != nil {
		return nil, failure
	}
	projects, err := fetchProjects(ctx, client)
	if failure := tokenFailureOf(err, tokenRevoked); failure != nil {
		return nil, failure
	}
	if !slices.ContainsFunc(projects, func(p project) bool { return p.ID == teamID }) {
		return nil, &tokenFailure{Code: tokenNoTeam,
			Message: fmt.Sprintf("team %s, chosen at setup, is not in this organization: run `terma setup` to choose another", teamID)}
	}
	teams := make([]tokenTeam, 0, len(projects))
	for _, p := range projects {
		teams = append(teams, tokenTeam{ID: p.ID, Name: p.Name})
	}
	// Read last: listing the teams may itself have rotated the pair.
	cred := client.Credential()
	return &tokenGrant{
		AccessToken:    cred.AccessToken,
		ExpiresAt:      cred.ExpiresAt.UTC().Format(time.RFC3339),
		OrganizationID: cred.OrganizationID,
		TeamID:         teamID,
		Teams:          teams,
		MCPURL:         cfg.MCPURL,
	}, nil
}

// tokenFailureOf classifies err: a missing or rejected credential is signedOut's code
// (signed_out before any exchange, revoked after one), anything else unavailable.
func tokenFailureOf(err error, signedOut string) *tokenFailure {
	if err == nil {
		return nil
	}
	if wrongEnv, ok := errors.AsType[*auth.ErrWrongEnvironment](err); ok {
		return &tokenFailure{Code: tokenSignedOut, Message: wrongEnv.Error()}
	}
	if errors.Is(err, auth.ErrNotLoggedIn) {
		if signedOut == tokenRevoked {
			return &tokenFailure{Code: tokenRevoked, Message: "your Terma session has ended: run `terma setup` to sign in again", cause: err}
		}
		return &tokenFailure{Code: tokenSignedOut, Message: "not signed in to Terma: run `terma setup`"}
	}
	return &tokenFailure{Code: tokenUnavailable, Message: "Terma could not be reached to refresh the sign-in; try again shortly, or run `terma doctor`", cause: err}
}
