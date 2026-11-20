package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Error is a structured failure from the gateway; its Message names the fix.
type Error struct {
	StatusCode int
	Code       string
	// Reason is the error's stable reason within Code (AIP-193), "" when it has none.
	Reason    string
	Message   string
	RequestID string
	// RetryAfter is the wait the answer asked for before a retry, 0 when it named none.
	RetryAfter time.Duration
}

// The reasons the CLI acts on.
const (
	// ReasonOrgNotAccessible: the person's membership of the organization says no:
	// removed, deactivated, a suspended organization, or another product's. The only
	// answer that means access is lost.
	ReasonOrgNotAccessible = "ORG_NOT_ACCESSIBLE"
	// ReasonAuthorizationUnavailable: the gateway cannot confirm the team or organization
	// yet. Retryable: it never means the team is gone.
	ReasonAuthorizationUnavailable = "AUTHORIZATION_UNAVAILABLE"
	// ReasonRefreshSuperseded: a concurrent refresh of the same token won.
	ReasonRefreshSuperseded = "REFRESH_SUPERSEDED"
	// ReasonSignInRetired: the sign-in is of a kind the server no longer accepts.
	ReasonSignInRetired = "SIGN_IN_RETIRED"
)

// OrgNotAccessible reports whether the person has lost access to the organization.
func (e *Error) OrgNotAccessible() bool {
	return e.StatusCode == http.StatusForbidden && e.Reason == ReasonOrgNotAccessible
}

// Retryable reports whether the answer says nothing about access: try again later.
func (e *Error) Retryable() bool {
	return e.StatusCode == http.StatusServiceUnavailable || e.StatusCode == http.StatusTooManyRequests ||
		e.Reason == ReasonAuthorizationUnavailable
}

// Superseded reports whether a refresh lost to a concurrent one of the same token.
func (e *Error) Superseded() bool {
	return e.StatusCode == http.StatusConflict && e.Reason == ReasonRefreshSuperseded
}

// SignInRetired reports whether the server no longer accepts this kind of sign-in.
func (e *Error) SignInRetired() bool { return e.Reason == ReasonSignInRetired }

func (e *Error) Error() string {
	msg := e.Message
	// The shared gateway's remedies name its original CLI; give terma's commands instead.
	if e.Code == "INVALID_ARGUMENT" && strings.Contains(msg, "missing X-Mirador-Project header") {
		msg = "no team selected — run `terma setup` or pass --team"
	}
	msg = gatewayRemedies.Replace(msg)
	switch {
	case msg == "" && e.StatusCode != 0:
		msg = fmt.Sprintf("request failed with status %d", e.StatusCode)
	case msg == "":
		msg = "request failed"
	case e.Code == "" && e.StatusCode != 0:
		// No envelope: the status is the only other thing known about the failure.
		msg = fmt.Sprintf("%s (status %d)", msg, e.StatusCode)
	}
	var detail []string
	if e.Code != "" {
		detail = append(detail, e.Code)
	}
	if e.Reason != "" {
		detail = append(detail, e.Reason)
	}
	if e.RequestID != "" {
		detail = append(detail, "request_id="+e.RequestID)
	}
	if len(detail) == 0 {
		return msg
	}
	return fmt.Sprintf("%s (%s)", msg, strings.Join(detail, ", "))
}

// gatewayRemedies renames the commands the shared gateway's messages recommend.
var gatewayRemedies = strings.NewReplacer(
	"`mirador login`", "`terma setup`",
	"`mirador project list`", "`terma setup`",
)

// Unauthenticated reports whether the credential itself was rejected.
func (e *Error) Unauthenticated() bool { return e.StatusCode == http.StatusUnauthorized }

func parseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var envelope struct {
		Error struct {
			Code    string           `json:"code"`
			Reason  string           `json:"reason"`
			Message string           `json:"message"`
			Details []map[string]any `json:"details"`
		} `json:"error"`
	}
	apiErr := &Error{StatusCode: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		apiErr.Code = envelope.Error.Code
		apiErr.Reason = envelope.Error.Reason
		apiErr.Message = envelope.Error.Message
		for _, detail := range envelope.Error.Details {
			if id, ok := detail["request_id"].(string); ok {
				apiErr.RequestID = id
			}
		}
		return apiErr
	}
	// Not the gateway's envelope: keep a proxy's first line, bounded.
	apiErr.Message, _, _ = strings.Cut(strings.TrimSpace(string(body)), "\n")
	if len(apiErr.Message) > maxPlainErrorLen {
		apiErr.Message = apiErr.Message[:maxPlainErrorLen] + "…"
	}
	if apiErr.Message == "" || strings.HasPrefix(apiErr.Message, "<") {
		apiErr.Message = http.StatusText(resp.StatusCode)
	}
	return apiErr
}

// retryAfter reads a Retry-After header's seconds; anything else is no wait named.
func retryAfter(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0
	}
	return min(time.Duration(n)*time.Second, time.Minute)
}
