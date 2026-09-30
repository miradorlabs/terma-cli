package claude

import (
	"cmp"
	"context"

	"github.com/miradorlabs/terma-cli/internal/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func captureClaudeAccount(e hookrun.Env, r *hookrun.Repo, in *claudeHookInput) {
	e.CaptureFunding(r, in.SessionID, claudeTool, hookrun.EventSessionAccount, readFunding(r.Root))
}

// stopFailure is notification-only. Capture the documented category, never
// error_details or last_assistant_message (either can contain private text). What Codex
// said is captured elsewhere, from the rollout and under the prompt-export consent
// (captureCodexReplies) — never from a hook payload, and never as funding evidence.
// https://code.claude.com/docs/en/hooks#stopfailure
func stopFailure(ctx context.Context, env hookrun.Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil || !session.ValidID(in.SessionID) {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	// ~/.claude.json is read once: the same evidence is spooled as the account record and
	// names the account the limit is reported against. The id is taken first because
	// captureFunding stamps its own keys onto the evidence's map.
	evidence := readFunding(r.Root)
	accountID, owned := oauthAccountID(evidence.Attrs)
	env.CaptureFunding(r, in.SessionID, claudeTool, hookrun.EventSessionAccount, evidence)
	kind := in.Error
	switch kind {
	case "rate_limit", "overloaded", "authentication_failed", "oauth_org_not_allowed", "account_on_hold", "billing_error", "invalid_request", "model_not_found", "server_error", "max_output_tokens", "cloud_credential_error", "unknown":
	case "":
		return nil
	default:
		kind = hookrun.UnknownValue
	}
	attrs := map[string]any{
		hookrun.AttrTool: claudeTool, "error_type": kind, hookrun.AttrEvidenceSource: "claude_stop_failure", hookrun.AttrSchemaVersion: 1, hookrun.AttrVersion: env.Version,
	}
	if owned {
		attrs[hookrun.AttrAccountID] = accountID
	}
	env.EmitFor(r, spool.Event{Name: hookrun.EventSessionLimit, SessionID: in.SessionID, Repo: r.Name, Attrs: attrs})
	return nil
}

// claudeOAuthAccount returns the cached OAuth account id and the organization it
// is signed in to, and true, only when OAuth is the proven effective credential.
// The organization shares the account's gate because it is part of the same
// login: Pro/Max plans belong to the account, Team/Enterprise seats and Console
// billing to the organization, and one account can switch organizations without
// changing id (anthropics/claude-code#89966). Claude Code's env API key, auth
// token, cloud providers (Bedrock/Vertex/Foundry) and a VISIBLE externally
// supplied CLAUDE_CODE_OAUTH_TOKEN (which may belong to a different account than
// the cached profile) all outrank the stored OAuth login; and attribution
// proceeds only when the apiKeyHelper state is a positive "not_found" (a
// "configured" helper is a competing credential, and an "unknown" state — e.g.
// a symlinked settings.json the Lstat evidence reader rejects — cannot rule one
// out). Under any of those the cached accountUuid is not the proven funding
// owner and attribution is withheld. When Claude strips CLAUDE_CODE_OAUTH_TOKEN
// from the hook it is indistinguishable from an ordinary interactive login, so
// the cached profile stands as best-effort evidence (never asserted as proof —
// the backend treats account_id as evidence, not a settled payer). Reads
// ~/.claude.json once via ClaudeFunding; a caller that already holds the evidence
// uses oauthAccountID and does not read it again.
func claudeOAuthAccount(root string) (accountID, orgID string, ok bool) {
	attrs := readFunding(root).Attrs
	if accountID, ok = oauthAccountID(attrs); !ok {
		return "", "", false
	}
	orgID, _ = attrs[hookrun.AttrOrganizationID].(string)
	return accountID, orgID, true
}

func oauthAccountID(attrs map[string]any) (string, bool) {
	// One shared gate (OAuthAccountIsEffective) decides whether the cached OAuth
	// login is the proven credential — an env API key/auth token, a cloud provider, a
	// visible external OAuth token, or an apiKeyHelper that is not a positive "not_found"
	// all withhold it ("configured" is a competing credential; "unknown" means we could
	// not read the settings, e.g. a symlinked settings.json the Lstat-based evidence
	// reader rejects, and so cannot rule one out). ClaudeFunding applies the same gate to
	// the account email, so the id and email can never drift apart.
	if !oauthAccountIsEffective(attrs) {
		return "", false
	}
	id, ok := attrs[hookrun.AttrAccountID].(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}
