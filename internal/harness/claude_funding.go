package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ClaudeFunding reads the stored login's metadata and visible credential hints.
// These describe the hook's view, never the credential a request actually used.
// It does not read credential stores, run apiKeyHelper or contact a provider.
func ClaudeFunding(repoRoot string) FundingEvidence {
	e := FundingEvidence{Source: "claude_account", Status: "unreadable", Attrs: map[string]any{}}
	configPath, err := (Claude{}).ConfigPath()
	if err != nil {
		return e
	}
	accountPath := filepath.Join(filepath.Dir(configPath), ".claude.json")
	if strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return e
		}
		accountPath = filepath.Join(home, ".claude.json")
	}
	doc, status := readEvidenceJSON(accountPath)
	e.Status = status
	if status == "present" {
		var account map[string]json.RawMessage
		raw, ok := doc["oauthAccount"]
		if !ok || string(raw) == "null" {
			e.Status = "missing"
		} else if json.Unmarshal(raw, &account) != nil || account == nil {
			e.Status = "malformed"
		} else {
			for from, to := range map[string]string{
				"accountUuid": "account_id", "organizationUuid": "organization_id",
				"billingType": "billing_type", "organizationType": "organization_type", "seatTier": "seat_tier",
				"cachedExtraUsageDisabledReason": "extra_usage_disabled_reason",
				// Claude has no plan_type on the wire; its rate-limit tier is the closest plan signal (the
				// platform's entitlement PlanType is empty for Claude today). Role + org tier add seat context.
				"userRateLimitTier": "user_rate_limit_tier", "organizationRateLimitTier": "organization_rate_limit_tier",
				"organizationRole": "organization_role",
			} {
				copyEvidenceString(e.Attrs, account, from, to)
			}
			copyEvidenceBool(e.Attrs, account, "hasExtraUsageEnabled", "extra_usage_enabled")
			// The signed-in email is the per-user identity behind the account. Copied via validEmail
			// (not copyEvidenceString, whose evidenceLabel forbids '@'); emitted raw as account_email,
			// the platform's terma-cli identity convention (renamed to user.email downstream).
			var email string
			if json.Unmarshal(account["emailAddress"], &email) == nil && validEmail(email) {
				e.Attrs["account_email"] = email
			}
		}
	}
	e.Attrs["api_key_present"] = os.Getenv("ANTHROPIC_API_KEY") != ""
	e.Attrs["auth_token_present"] = os.Getenv("ANTHROPIC_AUTH_TOKEN") != ""
	for key, attr := range map[string]string{"CLAUDE_CODE_USE_BEDROCK": "bedrock_enabled", "CLAUDE_CODE_USE_VERTEX": "vertex_enabled", "CLAUDE_CODE_USE_FOUNDRY": "foundry_enabled"} {
		// Use the same truthiness Claude Code accepts (isOn: 1/true/yes, trimmed) so a "yes"-enabled
		// cloud route is detected here and the cached OAuth account is withheld — not just 1/true.
		e.Attrs[attr] = isOn(os.Getenv(key))
	}
	// Claude strips this variable from hooks: its absence tells us nothing.
	e.Attrs["oauth_token_visibility"] = "unavailable"
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		e.Attrs["oauth_token_present"] = true
		e.Attrs["oauth_token_visibility"] = "visible"
	}
	// This is presence in files, not effective settings: --settings and MDM
	// overrides are not observable in a hook. Never execute or export the command.
	e.Attrs["hint_scope"] = "hook_env_and_settings_files"
	paths := []string{configPath}
	if repoRoot != "" {
		paths = append(paths, filepath.Join(repoRoot, ".claude", "settings.json"), filepath.Join(repoRoot, ".claude", "settings.local.json"))
	}
	helper := "not_found"
	for _, path := range paths {
		settings, status := readEvidenceJSON(path)
		if status != "present" && status != "missing" {
			if helper != "configured" {
				helper = "unknown"
			}
			continue
		}
		if raw, ok := settings["apiKeyHelper"]; ok {
			var command string
			if json.Unmarshal(raw, &command) != nil {
				if helper != "configured" {
					helper = "unknown"
				}
			} else if strings.TrimSpace(command) != "" {
				helper = "configured"
			}
		}
	}
	e.Attrs["api_key_helper_state"] = helper
	// The signed-in email is the cached OAuth login's. When the session is actually
	// using a different credential (an env API key or auth token, a cloud provider, a
	// visible external OAuth token, or a configured apiKeyHelper), that email is not
	// this session's identity — attributing it would tie the cached login to a session
	// it did not run, and on a shared machine (e.g. CI with a baked-in ~/.claude.json)
	// that email can be a different person entirely. Withhold it exactly where account_id
	// attribution is withheld downstream. account_id stays as raw evidence (a UUID beside
	// the credential hints that let the consumer judge it); the email is PII, so it does not.
	if !OAuthAccountIsEffective(e.Attrs) {
		delete(e.Attrs, "account_email")
	}
	return e
}

// OAuthAccountIsEffective reports whether the cached OAuth login is the credential the
// session is actually using: no env API key or auth token, no cloud provider
// (Bedrock/Vertex/Foundry), no visible external OAuth token, and a positively absent
// apiKeyHelper ("not_found" — "configured" is a competing credential and "unknown"
// cannot rule one out). Callers withhold the cached account's identity when it is false.
// It reads the credential-presence hints already in a ClaudeFunding evidence map.
func OAuthAccountIsEffective(attrs map[string]any) bool {
	for _, override := range []string{"api_key_present", "auth_token_present", "bedrock_enabled", "vertex_enabled", "foundry_enabled", "oauth_token_present"} {
		if on, _ := attrs[override].(bool); on {
			return false
		}
	}
	state, _ := attrs["api_key_helper_state"].(string)
	return state == "not_found"
}
