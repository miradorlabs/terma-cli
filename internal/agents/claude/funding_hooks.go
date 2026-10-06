package claude

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// claudeAccountStateDir holds the stored login each session was seen using.
var claudeAccountStateDir = hookrun.AgentStateDir(name, "accounts")

// claudeLoginCommand opens the transcript record of a /login run in the session itself.
const claudeLoginCommand = "<command-name>/login</command-name>"

func captureClaudeAccount(e hookrun.Env, r *hookrun.Repo, in *claudeHookInput) {
	// A new process reads the stored login afresh; a cleared or compacted session keeps its own.
	repin := in.Source == "startup" || in.Source == "resume"
	f := sessionFunding(e, r.Root, in.SessionID, in.TranscriptPath, repin)
	e.CaptureFunding(r, in.SessionID, claudeTool, semconv.TermaSessionAccountEvent, f.FundingEvidence)
}

// accountPin is the account evidence a session was pinned to, and when.
type accountPin struct {
	Status string         `json:"status"`
	Attrs  map[string]any `json:"attrs"`
	At     time.Time      `json:"at"`
}

// sessionFunding is readFunding for one session. ~/.claude.json follows the machine's latest
// /login, while a running session keeps the credential it started with: so the account first
// seen is kept until the session's own /login, or repin when a new process starts it.
func sessionFunding(e hookrun.Env, root, sessionID, transcript string, repin bool) claudeFunding {
	// Taken before the stored login is read, so a /login recorded after that read stays after the pin.
	now := e.Time()
	f := readFunding(root)
	dir := filepath.Join(e.StateDir, claudeAccountStateDir)
	path := filepath.Join(dir, hookrun.EvidenceID(sessionID)+".json")
	// Read without the lock, which is a try-lock: a pin is replaced atomically, and a hook that
	// loses the race must still hold the session's account.
	var pin accountPin
	b, readErr := os.ReadFile(path)
	if !repin && readErr == nil && json.Unmarshal(b, &pin) == nil && pin.Attrs[semconv.TermaAccountIDKey] != nil {
		if sameAccount(pin.Attrs, f.Attrs) {
			// Moving the pin's time on, at most once a minute, retires a /login that kept the
			// account: a later login elsewhere must not read as this session's.
			if now.Sub(pin.At) < time.Minute {
				return f
			}
		} else if !loggedInSince(transcript, sessionID, pin.At) {
			// Touched, at most hourly, so the sweep keeps a pin its session still holds.
			if info, err := os.Stat(path); err == nil && now.Sub(info.ModTime()) > time.Hour {
				_ = os.Chtimes(path, now, now)
			}
			f.Status, f.Attrs = pin.Status, pin.Attrs
			return f
		}
	}
	if f.Attrs[semconv.TermaAccountIDKey] == nil {
		if repin {
			_ = os.Remove(path)
		}
		return f
	}
	// From here the stored login is the session's; a busy lock means another of its hooks is
	// pinning it.
	if os.MkdirAll(dir, 0o700) != nil {
		return f
	}
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return f
	}
	defer unlock()
	// The email stays out of state, so a held session's evidence goes without one.
	stored := maps.Clone(f.Attrs)
	delete(stored, semconv.UserEmailKey)
	if b, err := json.Marshal(accountPin{Status: f.Status, Attrs: stored, At: now}); err == nil {
		if err := hookrun.WriteState(path, b); err != nil {
			e.Logf("claude account: %v", err)
		}
	}
	if os.IsNotExist(readErr) {
		hookrun.PruneState(dir, now.Add(-spool.MaxAge))
	}
	return f
}

func sameAccount(a, b map[string]any) bool {
	return a[semconv.TermaAccountIDKey] == b[semconv.TermaAccountIDKey] &&
		a[semconv.TermaAccountOrganizationIDKey] == b[semconv.TermaAccountOrganizationIDKey]
}

// loggedInSince reports whether the transcript records the session's own /login after since.
func loggedInSince(transcript, sessionID string, since time.Time) bool {
	if transcript == "" {
		return false
	}
	buf, err := readTranscriptTail(transcript)
	if err != nil {
		return false
	}
	for line := range bytes.Lines(buf) {
		if !bytes.Contains(line, []byte(claudeLoginCommand)) {
			continue
		}
		// Content is a string only for what the developer typed; the command quoted in a tool
		// result sits in an array and fails to decode.
		var rec struct {
			Type      string    `json:"type"`
			SessionID string    `json:"sessionId"`
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) == nil && rec.Type == "user" && rec.SessionID == sessionID &&
			strings.HasPrefix(rec.Message.Content, claudeLoginCommand) && rec.Timestamp.After(since) {
			return true
		}
	}
	return false
}

// claudeLimitStateDir records when each session's failures were last reported.
var claudeLimitStateDir = hookrun.AgentStateDir(name, "limits")

// stopFailure spools the documented error category as error.type, never error_details or
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
	if spoolLimit(env, r, in, in.Error, sourceClaudeStopFailure) {
		markLimit(env, in.SessionID)
	}
	return nil
}

// limitFromTranscript spools the session's last API error unless a report followed it: Claude
// Code signals a headless run's StopFailure hook as it starts, often before terma can spool
// anything, but awaits SessionEnd.
func limitFromTranscript(env hookrun.Env, r *hookrun.Repo, in *claudeHookInput) {
	at, kind, ok := lastAPIError(in.TranscriptPath, in.SessionID)
	// Past the sweep's retention its marker may be gone, so the error cannot be told unreported.
	if !ok || env.Time().Sub(at) > spool.MaxAge {
		return
	}
	path := limitMarker(env, in.SessionID)
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	// A busy lock is the same session's end from a second install of the hooks.
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var reported time.Time
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &reported) == nil && !reported.Before(at) {
		return
	}
	if spoolLimit(env, r, in, kind, sourceClaudeTranscript) {
		markLimit(env, in.SessionID)
	}
}

// spoolLimit reports whether the failure is settled: its limit spooled, or no category to send,
// so the session's end does not make one up.
func spoolLimit(env hookrun.Env, r *hookrun.Repo, in *claudeHookInput, kind, source string) bool {
	// ~/.claude.json is read once for both events; the id is taken first because CaptureFunding
	// stamps its own keys onto the evidence's map.
	funding := sessionFunding(env, r.Root, in.SessionID, in.TranscriptPath, false)
	accountID, owned := funding.accountID()
	env.CaptureFunding(r, in.SessionID, claudeTool, semconv.TermaSessionAccountEvent, funding.FundingEvidence)
	switch kind {
	case "rate_limit", "overloaded", "authentication_failed", "oauth_org_not_allowed", "account_on_hold", "billing_error", "invalid_request", "model_not_found", "server_error", "max_output_tokens", "cloud_credential_error":
	case "":
		return true
	default:
		kind = semconv.ErrorTypeOther
	}
	attrs := map[string]any{
		semconv.GenAIMainAgentNameKey: claudeTool, semconv.ErrorTypeKey: kind, semconv.TermaEvidenceSourceKey: source,
	}
	if owned {
		attrs[semconv.TermaAccountIDKey] = accountID
	}
	return env.EmitFor(r, spool.Event{Name: semconv.TermaSessionLimitEvent, SessionID: in.SessionID, Attrs: attrs})
}

func limitMarker(env hookrun.Env, sessionID string) string {
	return filepath.Join(env.StateDir, claudeLimitStateDir, hookrun.EvidenceID(sessionID)+".json")
}

// markLimit records that the session's failures up to now are reported.
func markLimit(env hookrun.Env, sessionID string) {
	path := limitMarker(env, sessionID)
	b, err := json.Marshal(env.Time())
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o700)
	}
	if err == nil {
		err = hookrun.WriteState(path, b)
	}
	if err != nil {
		env.Logf("claude limit: %v", err)
	}
}

// lastAPIError returns the time and category of the session's last API error in the transcript.
func lastAPIError(transcript, sessionID string) (time.Time, string, bool) {
	if transcript == "" {
		return time.Time{}, "", false
	}
	buf, err := readTranscriptTail(transcript)
	if err != nil {
		return time.Time{}, "", false
	}
	var at time.Time
	var kind string
	for line := range bytes.Lines(buf) {
		if !bytes.Contains(line, []byte(`"isApiErrorMessage":true`)) {
			continue
		}
		var rec struct {
			SessionID  string    `json:"sessionId"`
			Timestamp  time.Time `json:"timestamp"`
			IsAPIError bool      `json:"isApiErrorMessage"`
			Error      string    `json:"error"`
		}
		if json.Unmarshal(line, &rec) == nil && rec.IsAPIError && rec.SessionID == sessionID {
			at, kind = rec.Timestamp, rec.Error
		}
	}
	return at, kind, !at.IsZero()
}

// oauthAccount returns the account and organization only when OAuth is the effective credential
// (oauthAccountIsEffective); the organization shares the gate because one account can switch
// organizations without changing id. It is evidence, never a settled payer.
func (f claudeFunding) oauthAccount() (accountID, orgID string, ok bool) {
	if accountID, ok = f.accountID(); !ok {
		return "", "", false
	}
	orgID, _ = f.Attrs[semconv.TermaAccountOrganizationIDKey].(string)
	return accountID, orgID, true
}

func (f claudeFunding) accountID() (string, bool) {
	// The same gate withholds the email in readFunding, so the id and the email never drift apart.
	if !f.oauthEffective {
		return "", false
	}
	id, ok := f.Attrs[semconv.TermaAccountIDKey].(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}
