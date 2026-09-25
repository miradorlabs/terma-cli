package hookrun

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// evidenceState contains only a hash, never credential or provider file contents.
type evidenceState struct {
	Hash string    `json:"hash"`
	At   time.Time `json:"at"`
}

// captureFunding spools one piece of funding evidence for the session unless the same
// evidence was spooled within the heartbeat. The handler resolves the repository once
// and hands it to every capture it runs, as captureObservation's callers do.
func (e Env) captureFunding(r *repo, id, tool, name string, evidence harness.FundingEvidence) {
	if e.Spool == nil || !session.ValidID(id) {
		return
	}
	attrs := evidence.Attrs
	if attrs == nil {
		attrs = map[string]any{}
	}
	attrs[attrTool], attrs[attrVersion] = tool, e.Version
	attrs[attrEvidenceSource], attrs[attrEvidenceStatus] = evidence.Source, evidence.Status
	attrs[attrSchemaVersion] = 1
	if !evidence.SourceTime.IsZero() {
		attrs["source_time"] = evidence.SourceTime.UTC().Format(time.RFC3339Nano)
	}
	// Include project routing in the hash: a resumed session may move projects.
	attrs[AttrProjectID] = r.projectID
	r.stampWorktree(attrs)
	raw, err := json.Marshal(attrs)
	if err != nil {
		return
	}
	hash := sha256.Sum256(raw)
	key := sha256.Sum256([]byte(tool + "\x00" + name + "\x00" + id))
	dir, err := config.Dir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, fundingStateDir)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	path := filepath.Join(dir, hex.EncodeToString(key[:16])+".json")
	unlock, err := lockEvidence(path + ".lock")
	if err != nil {
		return
	} // Another invocation is already capturing this session.
	defer unlock()
	var prev evidenceState
	fresh := false
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &prev)
	} else {
		fresh = os.IsNotExist(err)
	}
	next := evidenceState{Hash: hex.EncodeToString(hash[:]), At: e.now()}
	if prev.Hash == next.Hash && !next.At.Before(prev.At) && next.At.Sub(prev.At) < quotaHeartbeat {
		return
	}
	ev := spool.Event{Time: e.now(), Name: name, SessionID: id, Repo: r.name, Attrs: attrs}
	if e.Spool.Append(ev) != nil {
		return
	} // Retry a failed append at the next hook.
	data, _ := json.Marshal(next)
	_ = writeState(path, data)
	if fresh {
		pruneQuotaState(dir, next.At.Add(-snapshotStateRetention))
	}
}

func (e Env) captureClaudeAccount(r *repo, in *claudeHookInput) {
	e.captureFunding(r, in.SessionID, claudeTool, EventSessionAccount, harness.ClaudeFunding(r.root))
}

// StopFailure is notification-only. Capture the documented category, never
// error_details or last_assistant_message (either can contain private text). What Codex
// said is captured elsewhere, from the rollout and under the prompt-export consent
// (captureCodexReplies) — never from a hook payload, and never as funding evidence.
// https://code.claude.com/docs/en/hooks#stopfailure
func StopFailure(ctx context.Context, env Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil || !session.ValidID(in.SessionID) {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		return nil
	}
	// ~/.claude.json is read once: the same evidence is spooled as the account record and
	// names the account the limit is reported against. The id is taken first because
	// captureFunding stamps its own keys onto the evidence's map.
	evidence := harness.ClaudeFunding(r.root)
	accountID, owned := oauthAccountID(evidence.Attrs)
	env.captureFunding(r, in.SessionID, claudeTool, EventSessionAccount, evidence)
	kind := in.Error
	switch kind {
	case "rate_limit", "overloaded", "authentication_failed", "oauth_org_not_allowed", "account_on_hold", "billing_error", "invalid_request", "model_not_found", "server_error", "max_output_tokens", "cloud_credential_error", "unknown":
	case "":
		return nil
	default:
		kind = unknownValue
	}
	attrs := map[string]any{
		attrTool: claudeTool, "error_type": kind, attrEvidenceSource: "claude_stop_failure", attrSchemaVersion: 1, attrVersion: env.Version,
	}
	if owned {
		attrs[attrAccountID] = accountID
	}
	env.emitFor(r, spool.Event{Name: EventSessionLimit, SessionID: in.SessionID, Repo: r.name, Attrs: attrs})
	return nil
}

// claudeOAuthAccountID returns the cached OAuth account id, and true, only when
// OAuth is the proven effective credential. Claude Code's env API key, auth
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
func claudeOAuthAccountID(root string) (string, bool) {
	return oauthAccountID(harness.ClaudeFunding(root).Attrs)
}

func oauthAccountID(attrs map[string]any) (string, bool) {
	// One shared gate (harness.OAuthAccountIsEffective) decides whether the cached OAuth
	// login is the proven credential — an env API key/auth token, a cloud provider, a
	// visible external OAuth token, or an apiKeyHelper that is not a positive "not_found"
	// all withhold it ("configured" is a competing credential; "unknown" means we could
	// not read the settings, e.g. a symlinked settings.json the Lstat-based evidence
	// reader rejects, and so cannot rule one out). ClaudeFunding applies the same gate to
	// the account email, so the id and email can never drift apart.
	if !harness.OAuthAccountIsEffective(attrs) {
		return "", false
	}
	id, ok := attrs[attrAccountID].(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}
