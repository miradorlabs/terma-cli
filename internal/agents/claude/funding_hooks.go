package claude

import (
	"cmp"
	"context"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func captureClaudeAccount(e hookrun.Env, r *hookrun.Repo, in *claudeHookInput) {
	e.CaptureFunding(r, in.SessionID, claudeTool, hookrun.EventSessionAccount, readFunding(r.Root))
}

// stopFailure spools the documented error category, never error_details or
// last_assistant_message: either can hold private text.
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
	// ~/.claude.json is read once for both events; the id is taken first because CaptureFunding
	// stamps its own keys onto the evidence's map.
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

// claudeOAuthAccount returns the cached OAuth account and organization only when OAuth is the
// effective credential (oauthAccountIsEffective); the organization shares the gate because one
// account can switch organizations without changing id. It is evidence, never a settled payer.
func claudeOAuthAccount(root string) (accountID, orgID string, ok bool) {
	attrs := readFunding(root).Attrs
	if accountID, ok = oauthAccountID(attrs); !ok {
		return "", "", false
	}
	orgID, _ = attrs[hookrun.AttrOrganizationID].(string)
	return accountID, orgID, true
}

func oauthAccountID(attrs map[string]any) (string, bool) {
	// The same gate withholds the email in readFunding, so the id and the email never drift apart.
	if !oauthAccountIsEffective(attrs) {
		return "", false
	}
	id, ok := attrs[hookrun.AttrAccountID].(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}
