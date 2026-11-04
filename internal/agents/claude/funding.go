package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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

// claudeFunding is the stored login as a hook sees it: the evidence to spool, the route the
// session's settings configure, and whether that login is the session's credential
// (oauthEffective).
type claudeFunding struct {
	hookrun.FundingEvidence
	route          claudeRoute
	oauthEffective bool
}

// claudeRoute is what a hook sees of how Claude Code reaches its model: the env blocks of the
// settings files, highest precedence first, and whether one configures an apiKeyHelper.
type claudeRoute struct {
	envs   []map[string]json.RawMessage
	helper bool
	// unreadable is a settings file that exists but cannot be read, which may hide either.
	unreadable bool
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
	route := readClaudeRoute(repoRoot, configPath)
	f := claudeFunding{FundingEvidence: e, route: route, oauthEffective: oauthAccountIsEffective(route)}
	// The cached login's email is withheld wherever the session may use another credential: it would
	// name a person who did not run the session (a CI machine's baked-in ~/.claude.json).
	if !f.oauthEffective {
		delete(e.Attrs, semconv.UserEmailKey)
	}
	return f
}

// readClaudeRoute reads the settings files a session in repoRoot layers, highest precedence
// first: settings.local.json, the project's settings.json, then the user's. The environment
// outranks them all (lookup), except a machine-wide managed env block, which is not read.
// Presence in files, not effective settings: --settings and MDM overrides are not observable.
func readClaudeRoute(repoRoot, configPath string) claudeRoute {
	var paths []string
	if repoRoot != "" {
		paths = append(paths, filepath.Join(repoRoot, ".claude", "settings.local.json"), filepath.Join(repoRoot, ".claude", "settings.json"))
	}
	paths = append(paths, configPath)
	var r claudeRoute
	for _, path := range paths {
		settings, status := hookrun.ReadEvidenceJSON(path)
		if status != hookrun.StatusPresent && status != "missing" {
			r.unreadable = true
			continue
		}
		var env map[string]json.RawMessage
		if json.Unmarshal(settings["env"], &env) == nil {
			r.envs = append(r.envs, env)
		}
		// Never execute or export the command.
		if raw, ok := settings["apiKeyHelper"]; ok {
			var command string
			if json.Unmarshal(raw, &command) != nil || strings.TrimSpace(command) != "" {
				r.helper = true
			}
		}
	}
	return r
}

// lookup is key's value as the session sees it: the environment's, else the first settings
// env block's that sets it.
func (r claudeRoute) lookup(key string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	for _, env := range r.envs {
		var v string
		if json.Unmarshal(env[key], &v) == nil && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// claudeCloud is a cloud provider route: the switch that selects it, the variables that send
// it to another endpoint (an LLM gateway), the first set deciding, and the backend that serves it.
type claudeCloud struct {
	switchVar   string
	baseURLVars []string
	backend     string
}

// claudeClouds are the cloud provider routes terma.serving.backend names, in the order Claude
// Code checks their switches.
var claudeClouds = []claudeCloud{
	{"CLAUDE_CODE_USE_BEDROCK", []string{"ANTHROPIC_BEDROCK_BASE_URL", "AWS_ENDPOINT_URL_BEDROCK_RUNTIME", "AWS_ENDPOINT_URL_BEDROCK", "AWS_ENDPOINT_URL"},
		semconv.TermaServingBackendAwsBedrock},
	{"CLAUDE_CODE_USE_FOUNDRY", []string{"ANTHROPIC_FOUNDRY_BASE_URL"}, semconv.TermaServingBackendAzureFoundry},
	// Amazon Bedrock's Mantle endpoint.
	{"CLAUDE_CODE_USE_MANTLE", []string{"ANTHROPIC_BEDROCK_MANTLE_BASE_URL"}, semconv.TermaServingBackendAwsBedrock},
	{"CLAUDE_CODE_USE_VERTEX", []string{"ANTHROPIC_VERTEX_BASE_URL"}, semconv.TermaServingBackendGoogleVertex},
}

// cloudRoute is the first cloud provider route whose switch lookup finds on.
func cloudRoute(lookup func(string) string) (claudeCloud, bool) {
	i := slices.IndexFunc(claudeClouds, func(c claudeCloud) bool { return isOn(lookup(c.switchVar)) })
	if i < 0 {
		return claudeCloud{}, false
	}
	return claudeClouds[i], true
}

// firstPartyKey reports whether lookup finds a first-party key or token, which the session
// uses before the stored login.
func firstPartyKey(lookup func(string) string) bool {
	return lookup("ANTHROPIC_API_KEY") != "" || lookup("ANTHROPIC_AUTH_TOKEN") != "" || lookup("CLAUDE_CODE_OAUTH_TOKEN") != ""
}

// oauthAccountIsEffective reports whether the cached OAuth login is the session's credential: no
// key or token, no cloud provider, and apiKeyHelper positively not configured. Claude Code strips
// CLAUDE_CODE_OAUTH_TOKEN from hooks' environment, so only a settings file can show it.
func oauthAccountIsEffective(r claudeRoute) bool {
	_, cloud := cloudRoute(r.lookup)
	return !r.unreadable && !r.helper && !cloud && !firstPartyKey(r.lookup)
}
