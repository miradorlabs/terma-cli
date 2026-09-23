package harness

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const codexTailLimit = 1 << 20

// CodexOAuthAccountID returns the ChatGPT account id Codex is signed in with, and true, when the
// effective auth route is the ChatGPT subscription: auth_mode "chatgpt" with no OPENAI_API_KEY in the
// auth.json. On the API-key route the cached tokens.account_id is not the paying identity, so this
// returns "", false — the mirror of claudeOAuthAccountID's api-key/bedrock/vertex gate. The auth.json
// is the source of truth: an exported OPENAI_API_KEY alone does NOT authenticate Codex's built-in
// provider (that needs `codex login --with-api-key`, which persists auth_mode/OPENAI_API_KEY to the
// file), so an unused shell var must not suppress a genuine subscription attribution. The id is
// evidence of who a subscription session belongs to, never asserted for API metering.
func CodexOAuthAccountID() (string, bool) {
	home, err := codexHome()
	if err != nil {
		return "", false
	}
	doc, status := readEvidenceJSON(filepath.Join(home, "auth.json"))
	if status != "present" {
		return "", false
	}
	var apiKey string
	if json.Unmarshal(doc["OPENAI_API_KEY"], &apiKey) == nil && apiKey != "" {
		return "", false
	}
	// Require a positively-decoded chatgpt auth_mode: an absent/null/malformed mode is not proof of
	// the subscription route, so it withholds rather than falling through to the cached account.
	var mode string
	if json.Unmarshal(doc["auth_mode"], &mode) != nil || mode != "chatgpt" {
		return "", false
	}
	var tokens struct {
		AccountID string `json:"account_id"`
	}
	// Bound and shape-check the id before exporting it: readEvidenceJSON permits a 2 MiB file, and an
	// unvalidated account_id would ride verbatim into every quota spool entry (a malformed/huge value
	// could blow the 16 MiB spool budget). evidenceLabel is the same guard other exported ids use.
	if json.Unmarshal(doc["tokens"], &tokens) != nil || !evidenceLabel.MatchString(tokens.AccountID) {
		return "", false
	}
	return tokens.AccountID, true
}

// CodexOAuthUser returns the ChatGPT per-user identity Codex is signed in with — the stable, opaque
// user_id and the (mutable) login email — and true, when the effective auth route is the ChatGPT
// subscription (the same gate as CodexOAuthAccountID). Both matter because Codex's native OTel export
// exposes NEITHER as a per-user id: it emits only the shared workspace account_id and the mutable
// user.email. The stable user_id lives solely in the id_token JWT here, so this funding record is the
// only channel that can give the platform a durable Codex principal. Read from the auth.json id_token,
// never asserted on the API-key route. Emitted raw (the platform hashes/renames); never the git
// enduser.id, which is user-set and unreliable.
func CodexOAuthUser() (email, userID string, ok bool) {
	home, err := codexHome()
	if err != nil {
		return "", "", false
	}
	doc, status := readEvidenceJSON(filepath.Join(home, "auth.json"))
	if status != "present" {
		return "", "", false
	}
	var apiKey string
	if json.Unmarshal(doc["OPENAI_API_KEY"], &apiKey) == nil && apiKey != "" {
		return "", "", false
	}
	var mode string
	if json.Unmarshal(doc["auth_mode"], &mode) != nil || mode != "chatgpt" {
		return "", "", false
	}
	var tokens struct {
		IDToken string `json:"id_token"`
	}
	if json.Unmarshal(doc["tokens"], &tokens) != nil || tokens.IDToken == "" {
		return "", "", false
	}
	email, userID = codexIDClaims(tokens.IDToken)
	return email, userID, true
}

// codexIDClaims decodes a JWT id_token payload (signature NOT verified — the token is Codex's own
// local credential used purely as identity evidence, never a security boundary here). Returns the
// verified login email and the stable opaque `user_id` (the OpenAI-namespaced claim), each "" when
// absent or implausible; both are shape-checked before they can ride into a spool entry verbatim.
func codexIDClaims(idToken string) (email, userID string) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
		Auth     struct {
			UserID string `json:"user_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", ""
	}
	if claims.Verified && validEmail(claims.Email) {
		email = claims.Email
	}
	if evidenceLabel.MatchString(claims.Auth.UserID) {
		userID = claims.Auth.UserID
	}
	return email, userID
}

func codexRolloutName(rel, id string) bool {
	clean := filepath.ToSlash(filepath.Clean(rel))
	return (strings.HasPrefix(clean, "sessions/") || strings.HasPrefix(clean, "archived_sessions/")) &&
		strings.HasPrefix(filepath.Base(rel), "rollout-") && strings.HasSuffix(rel, "-"+id+".jsonl")
}

func findCodexRollout(ctx context.Context, root *os.Root, dir, id string, depth int, budget *int, readFailed *bool) string {
	if depth > 3 || *budget <= 0 || ctx.Err() != nil {
		return ""
	}
	f, err := root.OpenFile(dir, os.O_RDONLY|evidenceOpenFlags, 0)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			*readFailed = true
		}
		return ""
	}
	defer f.Close()
	for *budget > 0 && ctx.Err() == nil {
		entries, err := f.ReadDir(min(128, *budget))
		for _, ent := range entries {
			if *budget <= 0 || ctx.Err() != nil {
				return ""
			}
			*budget--
			path := filepath.Join(dir, ent.Name())
			if ent.IsDir() {
				if found := findCodexRollout(ctx, root, path, id, depth+1, budget, readFailed); found != "" {
					return found
				}
			} else if ent.Type().IsRegular() && codexRolloutName(path, id) {
				return path
			}
		}
		if err != nil {
			if err != io.EOF {
				*readFailed = true
			}
			break
		}
	}
	return ""
}

// openCodexRollout opens one thread's rollout through a confined root, for every reader
// of it (funding, replies, the spawn record). The hook's transcript_path is a hint, not
// permission to read an arbitrary file: the path must sit under CODEX_HOME's session
// directories, must not be reached through a symlink, and the file's own session_meta
// must name this thread. Only plain JSONL is supported. A nil file comes with the
// reason as the status — unsupported_path, unsupported, session_mismatch, unreadable,
// search_limit, missing — which a caller reports rather than treats as an error.
func openCodexRollout(ctx context.Context, sessionID, transcript string) (*os.File, string) {
	status := "missing"
	if !evidenceLabel.MatchString(sessionID) || strings.ContainsAny(sessionID, `/\`) {
		status = "invalid_session"
		return nil, status
	}
	home, err := codexHome()
	if err != nil {
		status = "unreadable"
		return nil, status
	}
	home, err = filepath.Abs(home)
	if err != nil {
		status = "unreadable"
		return nil, status
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			status = "unreadable"
		}
		return nil, status
	}
	defer func() { _ = root.Close() }()
	rel := ""
	if transcript != "" {
		path := transcript
		if !filepath.IsAbs(path) {
			path = filepath.Join(home, path)
		}
		rel, err = filepath.Rel(home, path)
		if err != nil || !codexRolloutName(rel, sessionID) {
			status = "unsupported_path"
			return nil, status
		}
	} else {
		// Older hooks may omit transcript_path. Bound discovery as well as the read.
		budget := 4096
		readFailed := false
		for _, dir := range []string{"sessions", "archived_sessions"} {
			rel = findCodexRollout(ctx, root, dir, sessionID, 0, &budget, &readFailed)
			if rel != "" {
				break
			}
		}
		if rel == "" {
			if budget <= 0 || ctx.Err() != nil {
				status = "search_limit"
			} else if readFailed {
				status = "unreadable"
			}
			return nil, status
		}
	}
	// Reject symlinks and non-regular files before opening (no FIFO can block a hook).
	st, err := root.Lstat(rel)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			status = "unreadable"
		}
		return nil, status
	}
	if !st.Mode().IsRegular() {
		status = "unsupported"
		return nil, status
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|evidenceOpenFlags, 0)
	if err != nil {
		status = "unreadable"
		return nil, status
	}

	st, err = f.Stat()
	if err != nil {
		status = "unreadable"
		f.Close()
		return nil, status
	}
	if !st.Mode().IsRegular() {
		status = "unsupported"
		f.Close()
		return nil, status
	}
	// Read identity at the head; a matching filename alone is not a session join.
	head := make([]byte, 64<<10)
	n, _ := f.ReadAt(head, 0)
	line, _, complete := bytes.Cut(head[:n], []byte{'\n'})
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	if !complete || json.Unmarshal(line, &meta) != nil || meta.Type != "session_meta" || meta.Payload.ID != sessionID {
		status = "session_mismatch"
		f.Close()
		return nil, status
	}
	return f, "present"
}

func codexQuota(raw json.RawMessage, at time.Time) FundingEvidence {
	e := FundingEvidence{Source: "codex_rollout", Status: "unavailable", SourceTime: at, Attrs: map[string]any{}}
	if string(raw) == "null" {
		return e
	}
	var limits map[string]json.RawMessage
	if json.Unmarshal(raw, &limits) != nil || limits == nil || at.IsZero() {
		e.Status = "malformed"
		return e
	}
	e.Status = "present"
	for _, key := range []string{"plan_type", "limit_id", "rate_limit_reached_type"} {
		copyEvidenceString(e.Attrs, limits, key, key)
	}
	copyEvidenceBool(e.Attrs, limits, "spend_control_reached", "spend_control_reached")
	for _, window := range []string{"primary", "secondary"} {
		var fields map[string]json.RawMessage
		if json.Unmarshal(limits[window], &fields) != nil || fields == nil {
			continue
		}
		copyEvidenceNumber(e.Attrs, fields, "used_percent", window+"_used_pct", math.MaxFloat64, false)
		copyEvidenceNumber(e.Attrs, fields, "window_minutes", window+"_window_minutes", math.MaxInt32, true)
		copyEvidenceNumber(e.Attrs, fields, "resets_at", window+"_resets_at", math.MaxInt64, true)
	}
	var credits map[string]json.RawMessage
	if json.Unmarshal(limits["credits"], &credits) == nil && credits != nil {
		copyEvidenceBool(e.Attrs, credits, "has_credits", "has_credits")
		copyEvidenceBool(e.Attrs, credits, "unlimited", "credits_unlimited")
		var balance string
		if json.Unmarshal(credits["balance"], &balance) == nil && validCreditBalance(balance) {
			e.Attrs["credits_balance"] = balance // Preserve units and precision; never call it USD.
		}
	}
	return e
}
