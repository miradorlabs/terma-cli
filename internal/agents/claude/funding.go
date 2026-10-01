package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// readFunding reads the stored login's metadata and visible credential hints: the hook's view,
// never the credential a request used. It reads no credential store and runs no apiKeyHelper.
func readFunding(repoRoot string) harness.FundingEvidence {
	e := harness.FundingEvidence{Source: "claude_account", Status: "unreadable", Attrs: map[string]any{}}
	configPath, err := (exporter{}).ConfigPath()
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
	doc, status := harness.ReadEvidenceJSON(accountPath)
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
				// No plan_type on the wire; the rate-limit tier is the closest plan signal.
				"userRateLimitTier": "user_rate_limit_tier", "organizationRateLimitTier": "organization_rate_limit_tier",
				"organizationRole": "organization_role",
			} {
				harness.CopyEvidenceString(e.Attrs, account, from, to)
			}
			harness.CopyEvidenceBool(e.Attrs, account, "hasExtraUsageEnabled", "extra_usage_enabled")
			// CopyEvidenceString forbids '@', so the email goes through ValidEmail.
			var email string
			if json.Unmarshal(account["emailAddress"], &email) == nil && harness.ValidEmail(email) {
				e.Attrs["account_email"] = email
			}
		}
	}
	e.Attrs["api_key_present"] = os.Getenv("ANTHROPIC_API_KEY") != ""
	e.Attrs["auth_token_present"] = os.Getenv("ANTHROPIC_AUTH_TOKEN") != ""
	for key, attr := range map[string]string{"CLAUDE_CODE_USE_BEDROCK": "bedrock_enabled", "CLAUDE_CODE_USE_VERTEX": "vertex_enabled", "CLAUDE_CODE_USE_FOUNDRY": "foundry_enabled"} {
		// isOn's truthiness, so a "yes"-enabled cloud route withholds the cached OAuth account too.
		e.Attrs[attr] = isOn(os.Getenv(key))
	}
	// Claude Code strips this variable from hooks: its absence tells nothing.
	e.Attrs["oauth_token_visibility"] = "unavailable"
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		e.Attrs["oauth_token_present"] = true
		e.Attrs["oauth_token_visibility"] = "visible"
	}
	// Presence in files, not effective settings: --settings and MDM overrides are not observable.
	// Never execute or export the command.
	e.Attrs["hint_scope"] = "hook_env_and_settings_files"
	paths := []string{configPath}
	if repoRoot != "" {
		paths = append(paths, filepath.Join(repoRoot, ".claude", "settings.json"), filepath.Join(repoRoot, ".claude", "settings.local.json"))
	}
	helper := "not_found"
	for _, path := range paths {
		settings, status := harness.ReadEvidenceJSON(path)
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
	// The cached login's email is withheld wherever the session may use another credential: it would
	// name a person who did not run the session (a CI machine's baked-in ~/.claude.json).
	if !oauthAccountIsEffective(e.Attrs) {
		delete(e.Attrs, "account_email")
	}
	return e
}

// oauthAccountIsEffective reports whether the cached OAuth login is the session's credential: no
// env key or token, no cloud provider, no visible OAuth token, and apiKeyHelper positively "not_found".
func oauthAccountIsEffective(attrs map[string]any) bool {
	for _, override := range []string{"api_key_present", "auth_token_present", "bedrock_enabled", "vertex_enabled", "foundry_enabled", "oauth_token_present"} {
		if on, _ := attrs[override].(bool); on {
			return false
		}
	}
	state, _ := attrs["api_key_helper_state"].(string)
	return state == "not_found"
}
