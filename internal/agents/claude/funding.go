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

// claudeFunding is the stored login as a hook sees it: the evidence to spool, whether that
// login is the session's credential (oauthEffective), and the other credential the hook
// proves where it is not (proven).
type claudeFunding struct {
	hookrun.FundingEvidence
	oauthEffective bool
	proven         string
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
				"seatTier": semconv.TermaAccountSeatTierKey, "userRateLimitTier": semconv.TermaAccountRateLimitTierKey,
				"organizationRateLimitTier": semconv.TermaAccountOrganizationRateLimitTierKey,
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
	f := claudeFunding{FundingEvidence: e}
	f.proven, f.oauthEffective = effectiveCredential(repoRoot, configPath)
	// The cached login's email is withheld wherever the session may use another credential: it would
	// name a person who did not run the session (a CI machine's baked-in ~/.claude.json).
	if !f.oauthEffective {
		delete(e.Attrs, semconv.UserEmailKey)
	}
	return f
}

// credential is terma.account.credential for the session: account where the stored login is its
// credential and names an account, the credential the hook proves otherwise, "" when it cannot tell.
func (f claudeFunding) credential() string {
	if _, ok := f.accountID(); ok {
		return semconv.TermaAccountCredentialAccount
	}
	return f.proven
}

// effectiveCredential is the session's credential as far as a hook can prove it, in Claude Code's
// order: a cloud provider, then an env key or token or a configured apiKeyHelper (proven). Else oauth
// reports whether the cached OAuth login is the credential: no visible OAuth token, and apiKeyHelper
// positively not configured. Claude Code strips CLAUDE_CODE_OAUTH_TOKEN from hooks, so its absence
// tells nothing.
func effectiveCredential(repoRoot, configPath string) (proven string, oauth bool) {
	// isOn's truthiness, so a "yes"-enabled cloud route withholds the cached OAuth account too.
	if isOn(os.Getenv("CLAUDE_CODE_USE_BEDROCK")) || isOn(os.Getenv("CLAUDE_CODE_USE_VERTEX")) || isOn(os.Getenv("CLAUDE_CODE_USE_FOUNDRY")) {
		return semconv.TermaAccountCredentialCloud, false
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("ANTHROPIC_AUTH_TOKEN") != "" {
		return semconv.TermaAccountCredentialApiKey, false
	}
	// Presence in files, not effective settings: --settings and MDM overrides are not observable.
	// Never execute or export the command.
	paths := []string{configPath}
	if repoRoot != "" {
		paths = append(paths, filepath.Join(repoRoot, ".claude", "settings.json"), filepath.Join(repoRoot, ".claude", "settings.local.json"))
	}
	known := true
	for _, path := range paths {
		settings, status := hookrun.ReadEvidenceJSON(path)
		if status != "present" && status != "missing" {
			known = false
			continue
		}
		if raw, ok := settings["apiKeyHelper"]; ok {
			var command string
			if json.Unmarshal(raw, &command) != nil {
				known = false
			} else if strings.TrimSpace(command) != "" {
				return semconv.TermaAccountCredentialApiKey, false
			}
		}
	}
	// A visible OAuth token may be another account's.
	return "", known && os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == ""
}
