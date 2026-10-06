package codex

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

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

const codexTailLimit = 1 << 20

// oauthAccountID returns the ChatGPT account id when auth.json's route is the
// subscription (auth_mode "chatgpt", no OPENAI_API_KEY in the file). A shell
// OPENAI_API_KEY alone does not authenticate Codex, so it does not suppress this.
func oauthAccountID() (string, bool) {
	home, err := codexHome()
	if err != nil {
		return "", false
	}
	doc, status := hookrun.ReadEvidenceJSON(filepath.Join(home, "auth.json"))
	if status != "present" {
		return "", false
	}
	var apiKey string
	if json.Unmarshal(doc["OPENAI_API_KEY"], &apiKey) == nil && apiKey != "" {
		return "", false
	}
	// An absent or malformed mode is no proof of the subscription route.
	var mode string
	if json.Unmarshal(doc["auth_mode"], &mode) != nil || mode != "chatgpt" {
		return "", false
	}
	var tokens struct {
		AccountID string `json:"account_id"`
	}
	// Shape-checked: an unvalidated id would ride verbatim into every quota spool entry.
	if json.Unmarshal(doc["tokens"], &tokens) != nil || !hookrun.EvidenceLabel.MatchString(tokens.AccountID) {
		return "", false
	}
	return tokens.AccountID, true
}

// oauthEmail returns the ChatGPT login's email under oauthAccountID's gate: Codex's export
// carries only the shared workspace account_id, so the id_token is the one source of who
// holds the seat.
func oauthEmail() string {
	home, err := codexHome()
	if err != nil {
		return ""
	}
	doc, status := hookrun.ReadEvidenceJSON(filepath.Join(home, "auth.json"))
	if status != "present" {
		return ""
	}
	var apiKey string
	if json.Unmarshal(doc["OPENAI_API_KEY"], &apiKey) == nil && apiKey != "" {
		return ""
	}
	var mode string
	if json.Unmarshal(doc["auth_mode"], &mode) != nil || mode != "chatgpt" {
		return ""
	}
	var tokens struct {
		IDToken string `json:"id_token"`
	}
	if json.Unmarshal(doc["tokens"], &tokens) != nil || tokens.IDToken == "" {
		return ""
	}
	return codexIDEmail(tokens.IDToken)
}

// codexIDEmail decodes the id_token without verifying it: identity evidence, not a security
// boundary. The email is shape-checked before it reaches a spool entry.
func codexIDEmail(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
	}
	if json.Unmarshal(payload, &claims) != nil || !claims.Verified || !hookrun.ValidEmail(claims.Email) {
		return ""
	}
	return claims.Email
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
	f, err := root.OpenFile(dir, os.O_RDONLY|hookrun.EvidenceOpenFlags, 0)
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

// openCodexRollout opens a thread's rollout through a confined root: the path must sit
// under CODEX_HOME's session directories, not through a symlink, and its session_meta must
// name this thread. A nil file comes with the reason as status.
func openCodexRollout(ctx context.Context, sessionID, transcript string) (*os.File, string) {
	status := "missing"
	if !hookrun.EvidenceLabel.MatchString(sessionID) || strings.ContainsAny(sessionID, `/\`) {
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
		// Older hooks may omit transcript_path; discovery is bounded too.
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
	// Rejected before opening, so no FIFO can block a hook.
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
	f, err := root.OpenFile(rel, os.O_RDONLY|hookrun.EvidenceOpenFlags, 0)
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

func codexQuota(raw json.RawMessage, at time.Time) hookrun.FundingEvidence {
	e := hookrun.FundingEvidence{Source: sourceCodexRollout, Status: "unavailable", SourceTime: at, Attrs: map[string]any{}}
	if string(raw) == "null" {
		return e
	}
	var limits map[string]json.RawMessage
	if json.Unmarshal(raw, &limits) != nil || limits == nil || at.IsZero() {
		e.Status = "malformed"
		return e
	}
	e.Status = "present"
	hookrun.CopyEvidenceString(e.Attrs, limits, "plan_type", semconv.TermaAccountPlanTypeKey)
	hookrun.CopyEvidenceString(e.Attrs, limits, "rate_limit_reached_type", semconv.TermaRateLimitReachedTypeKey)
	hookrun.CopyEvidenceBool(e.Attrs, limits, "spend_control_reached", semconv.TermaSpendControlReachedKey)
	for _, w := range []struct{ name, used, minutes, resets string }{
		{"primary", semconv.TermaRateLimitPrimaryUsedPercentKey, semconv.TermaRateLimitPrimaryWindowMinutesKey, semconv.TermaRateLimitPrimaryResetsAtKey},
		{"secondary", semconv.TermaRateLimitSecondaryUsedPercentKey, semconv.TermaRateLimitSecondaryWindowMinutesKey, semconv.TermaRateLimitSecondaryResetsAtKey},
	} {
		var fields map[string]json.RawMessage
		if json.Unmarshal(limits[w.name], &fields) != nil || fields == nil {
			continue
		}
		hookrun.CopyEvidenceNumber(e.Attrs, fields, "used_percent", w.used, math.MaxFloat64, false)
		hookrun.CopyEvidenceNumber(e.Attrs, fields, "window_minutes", w.minutes, math.MaxInt32, true)
		hookrun.CopyEvidenceNumber(e.Attrs, fields, "resets_at", w.resets, math.MaxInt64, true)
	}
	var credits map[string]json.RawMessage
	if json.Unmarshal(limits["credits"], &credits) == nil && credits != nil {
		hookrun.CopyEvidenceBool(e.Attrs, credits, "has_credits", semconv.TermaCreditsPresentKey)
		hookrun.CopyEvidenceBool(e.Attrs, credits, "unlimited", semconv.TermaCreditsUnlimitedKey)
		var balance string
		if json.Unmarshal(credits["balance"], &balance) == nil && hookrun.ValidCreditBalance(balance) {
			e.Attrs[semconv.TermaCreditsBalanceKey] = balance // Preserve units and precision; never call it USD.
		}
	}
	return e
}
