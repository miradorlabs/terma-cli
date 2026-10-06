package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Values of terma.evidence.source.
const (
	sourceClaudeAccount     = "claude_account"
	sourceClaudeStatusline  = "claude_statusline"
	sourceClaudeStopFailure = "claude_stop_failure"
	sourceClaudeTranscript  = "claude_transcript"
)

// claudeFunding is the stored login as a hook sees it: the evidence to spool, and whether
// that login is the session's credential (oauthEffective).
type claudeFunding struct {
	hookrun.FundingEvidence
	oauthEffective bool
}

// readFunding reads the stored login's metadata and visible credential hints: the hook's view,
// never the credential a request used. It reads no credential store and runs no apiKeyHelper.
func readFunding(repoRoot string) claudeFunding {
	e := hookrun.FundingEvidence{Source: sourceClaudeAccount, Status: "unreadable", Attrs: map[string]any{}}
	configPath, err := (exporter{}).ConfigPath()
	if err != nil {
		return claudeFunding{FundingEvidence: e}
	}
	accountPath := filepath.Join(filepath.Dir(configPath), ".claude.json")
	if strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return claudeFunding{FundingEvidence: e}
		}
		accountPath = filepath.Join(home, ".claude.json")
	}
	doc, status := hookrun.ReadEvidenceJSON(accountPath)
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
				"accountUuid": semconv.TermaAccountIDKey, "organizationUuid": semconv.TermaAccountOrganizationIDKey,
				"billingType": semconv.TermaAccountBillingTypeKey, "organizationType": semconv.TermaAccountOrganizationTypeKey,
				"seatTier": semconv.TermaAccountSeatTierKey,
			} {
				hookrun.CopyEvidenceString(e.Attrs, account, from, to)
			}
			hookrun.CopyEvidenceBool(e.Attrs, account, "hasExtraUsageEnabled", semconv.TermaAccountExtraUsageEnabledKey)
			// CopyEvidenceString forbids '@', so the email goes through ValidEmail.
			var email string
			if json.Unmarshal(account["emailAddress"], &email) == nil && hookrun.ValidEmail(email) {
				e.Attrs[semconv.UserEmailKey] = email
			}
		}
	}
	f := claudeFunding{FundingEvidence: e, oauthEffective: oauthAccountIsEffective(repoRoot, configPath)}
	// The cached login's email is withheld wherever the session may use another credential: it would
	// name a person who did not run the session (a CI machine's baked-in ~/.claude.json).
	if !f.oauthEffective {
		delete(e.Attrs, semconv.UserEmailKey)
	}
	return f
}

// oauthAccountIsEffective reports whether the cached OAuth login is the session's credential: no
// env key or token, no cloud provider, no visible OAuth token, and apiKeyHelper positively not
// configured. Claude Code strips CLAUDE_CODE_OAUTH_TOKEN from hooks, so its absence tells nothing.
func oauthAccountIsEffective(repoRoot, configPath string) bool {
	// isOn's truthiness, so a "yes"-enabled cloud route withholds the cached OAuth account too.
	if os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("ANTHROPIC_AUTH_TOKEN") != "" || os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" ||
		isOn(os.Getenv("CLAUDE_CODE_USE_BEDROCK")) || isOn(os.Getenv("CLAUDE_CODE_USE_VERTEX")) || isOn(os.Getenv("CLAUDE_CODE_USE_FOUNDRY")) {
		return false
	}
	// Presence in files, not effective settings: --settings and MDM overrides are not observable.
	// Never execute or export the command.
	paths := []string{configPath}
	if repoRoot != "" {
		paths = append(paths, filepath.Join(repoRoot, ".claude", "settings.json"), filepath.Join(repoRoot, ".claude", "settings.local.json"))
	}
	for _, path := range paths {
		settings, status := hookrun.ReadEvidenceJSON(path)
		if status != "present" && status != "missing" {
			return false
		}
		if raw, ok := settings["apiKeyHelper"]; ok {
			var command string
			if json.Unmarshal(raw, &command) != nil || strings.TrimSpace(command) != "" {
				return false
			}
		}
	}
	return true
}
