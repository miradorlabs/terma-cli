package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codex_auth_chatgpt.json is a real ~/.codex/auth.json shape captured from a live Codex install
// (auth_mode "chatgpt", OPENAI_API_KEY null, tokens.account_id set), with the token strings replaced
// by placeholders and the account id anonymized. It pins CodexOAuthAccountID against the true file.
const wantChatGPTAccount = "a5c616f7-0e91-49d4-bcfb-000000000001"

func writeCodexHome(t *testing.T, authJSON []byte) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), authJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	t.Setenv("OPENAI_API_KEY", "")
	return home
}

func TestCodexOAuthAccountID_ChatGPTRoute(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "codex_auth_chatgpt.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeCodexHome(t, data)
	id, ok := CodexOAuthAccountID()
	if !ok || id != wantChatGPTAccount {
		t.Fatalf("CodexOAuthAccountID() = %q,%v; want %q,true", id, ok, wantChatGPTAccount)
	}
}

func TestCodexOAuthAccountID_APIKeyRoutesDenied(t *testing.T) {
	base := map[string]any{
		"tokens":    map[string]any{"account_id": wantChatGPTAccount},
		"auth_mode": "chatgpt",
	}
	// A key in the file is the API-key route: the cached OAuth account is not the payer.
	withFileKey := map[string]any{"OPENAI_API_KEY": "sk-not-a-real-key", "tokens": base["tokens"], "auth_mode": "chatgpt"}
	// A non-chatgpt auth_mode is the same story.
	apikeyMode := map[string]any{"OPENAI_API_KEY": nil, "tokens": base["tokens"], "auth_mode": "apikey"}

	for name, doc := range map[string]map[string]any{"file_api_key": withFileKey, "apikey_mode": apikeyMode} {
		t.Run(name, func(t *testing.T) {
			b, _ := json.Marshal(doc)
			writeCodexHome(t, b)
			if id, ok := CodexOAuthAccountID(); ok {
				t.Errorf("expected no attribution on API-key route, got %q", id)
			}
		})
	}
}

// An exported OPENAI_API_KEY alone does not authenticate Codex's built-in provider (that needs
// `codex login --with-api-key`, which persists to auth.json). A ChatGPT auth.json still names the
// paying identity, so a leftover shell var must not suppress a genuine subscription attribution.
func TestCodexOAuthAccountID_EnvKeyDoesNotSuppress(t *testing.T) {
	b, _ := json.Marshal(map[string]any{
		"tokens":    map[string]any{"account_id": wantChatGPTAccount},
		"auth_mode": "chatgpt",
	})
	writeCodexHome(t, b)
	t.Setenv("OPENAI_API_KEY", "sk-env-key")
	if id, ok := CodexOAuthAccountID(); !ok || id != wantChatGPTAccount {
		t.Fatalf("CodexOAuthAccountID() = %q,%v; want %q,true (env key must not suppress the file route)", id, ok, wantChatGPTAccount)
	}
}

// A malformed/oversized tokens.account_id (readEvidenceJSON permits a 2 MiB file) must not ride
// verbatim into every quota spool entry; it is shape-checked against evidenceLabel before export.
func TestCodexOAuthAccountID_RejectsMalformedID(t *testing.T) {
	for name, id := range map[string]string{
		"oversized":       strings.Repeat("a", 200),
		"illegal_char":    "acct id with spaces",
		"newline_payload": "a5c616f7\nmore",
	} {
		t.Run(name, func(t *testing.T) {
			b, _ := json.Marshal(map[string]any{
				"tokens":    map[string]any{"account_id": id},
				"auth_mode": "chatgpt",
			})
			writeCodexHome(t, b)
			if got, ok := CodexOAuthAccountID(); ok {
				t.Errorf("expected rejection of malformed account id, got %q", got)
			}
		})
	}
}

func TestCodexOAuthAccountID_MissingOrEmpty(t *testing.T) {
	// No file at all.
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("OPENAI_API_KEY", "")
	if _, ok := CodexOAuthAccountID(); ok {
		t.Error("expected no attribution when auth.json is absent")
	}
	// Present but no account id.
	writeCodexHome(t, []byte(`{"auth_mode":"chatgpt","tokens":{}}`))
	if _, ok := CodexOAuthAccountID(); ok {
		t.Error("expected no attribution when tokens.account_id is empty")
	}
	// A valid account id but an absent auth_mode is not proof of the chatgpt route -> withheld.
	writeCodexHome(t, []byte(`{"tokens":{"account_id":"`+wantChatGPTAccount+`"}}`))
	if _, ok := CodexOAuthAccountID(); ok {
		t.Error("expected no attribution when auth_mode is absent (unproven route)")
	}
	// A null/malformed auth_mode is likewise not a proven route.
	writeCodexHome(t, []byte(`{"auth_mode":null,"tokens":{"account_id":"`+wantChatGPTAccount+`"}}`))
	if _, ok := CodexOAuthAccountID(); ok {
		t.Error("expected no attribution when auth_mode is null")
	}
}
