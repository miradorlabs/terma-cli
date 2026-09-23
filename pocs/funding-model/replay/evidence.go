package replay

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/miradorlabs/terma-cli/pocs/funding-model/model"
)

func str(a map[string]json.RawMessage, k string) string {
	var s string
	_ = json.Unmarshal(a[k], &s)
	return s
}
func boolean(a map[string]json.RawMessage, k string) *bool {
	var b *bool
	if json.Unmarshal(a[k], &b) == nil {
		return b
	}
	if s := str(a, k); s == "true" || s == "false" {
		return model.Bool(s == "true")
	}
	return nil
}
func number(a map[string]json.RawMessage, k string) *float64 {
	var n *float64
	if json.Unmarshal(a[k], &n) == nil && n != nil && finite(*n) {
		return n
	}
	if s := str(a, k); s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil && finite(v) {
			return &v
		}
	}
	return nil
}
func yes(a map[string]json.RawMessage, k string) bool { b := boolean(a, k); return b != nil && *b }

// sessionAt never uses a Stop snapshot from after the call as its starting state.
// Fresh missing/unavailable snapshots supersede old populated snapshots.
func sessionAt(c Call, events []Event, organizationID string) (model.Session, *model.QuotaSnapshot, []string) {
	s := model.Session{ID: c.SessionID, UserID: c.UserID, Harness: c.Harness}
	var account, quota *Event
	for i := range events {
		ev := &events[i]
		if ev.SessionID != c.SessionID || str(ev.Attrs, "tool") != string(c.Harness) || ev.Time.After(c.At) {
			continue
		}
		switch ev.Name {
		case "terma.session.account":
			if account == nil || laterEvidence(ev, account) {
				account = ev
			}
		case "terma.session.quota":
			if quota == nil || laterEvidence(ev, quota) {
				quota = ev
			}
		case "terma.session.limit":
			// Production StopFailure supplies a category, not a budget scope.
			// A generic rate_limit cannot establish that paid credits are blocked.
			s.Limits = append(s.Limits, model.LimitEvent{At: ev.Time, Kind: str(ev.Attrs, "error_type"), Scope: "unknown"})
		}
	}
	warnings := []string{}
	if account != nil && c.At.Sub(account.Time) <= 30*time.Minute {
		a := account.Attrs
		s.Hints = model.Hints{APIKey: yes(a, "api_key_present"), AuthToken: yes(a, "auth_token_present"), APIKeyHelper: str(a, "api_key_helper_state") == "configured", CloudProvider: yes(a, "bedrock_enabled") || yes(a, "vertex_enabled") || yes(a, "foundry_enabled"), OAuthTokenEnv: yes(a, "oauth_token_present")}
		if str(a, "evidence_status") == "present" && c.AccountID != "" && str(a, "account_id") == c.AccountID && str(a, "organization_id") == organizationID {
			s.Account = &model.AccountSnapshot{BillingType: str(a, "billing_type"), OrganizationType: str(a, "organization_type"), SeatTier: str(a, "seat_tier"), ExtraUsageEnabled: boolean(a, "extra_usage_enabled"), ExtraUsageDisabledReason: str(a, "extra_usage_disabled_reason")}
		}
	}
	var q *model.QuotaSnapshot
	if quota != nil {
		a := quota.Attrs
		at := quota.Time
		usable := true
		if c.Harness == model.HarnessClaude {
			status := str(a, "evidence_status")
			usable = s.Account != nil && account != nil && !quota.Time.Before(account.Time) && (status == "" || status == "present")
		}
		if c.Harness == model.HarnessCodex {
			var err error
			at, err = time.Parse(time.RFC3339Nano, str(a, "source_time"))
			usable = err == nil && !at.After(quota.Time) && !at.After(c.At) && str(a, "evidence_status") == "present"
			if usable && c.At.Sub(at) <= 30*time.Minute && str(a, "plan_type") != "" {
				s.Account = &model.AccountSnapshot{PlanType: str(a, "plan_type"), HasCredits: boolean(a, "has_credits"), CreditsUnlimited: boolean(a, "credits_unlimited")}
			}
		}
		if usable && c.At.Sub(at) <= 15*time.Minute {
			q = &model.QuotaSnapshot{At: at}
			primary, secondary := "five_hour", "seven_day"
			if c.Harness == model.HarnessCodex {
				primary, secondary = "primary", "secondary"
			}
			window := func(name string) (*float64, time.Time) {
				reset := number(a, name+"_resets_at")
				if reset == nil || *reset >= float64(1<<63) {
					return nil, time.Time{}
				}
				t := time.Unix(int64(*reset), 0)
				if !t.After(c.At) {
					return nil, t
				}
				return number(a, name+"_used_pct"), t
			}
			q.FiveHourUsedPct, q.FiveHourResetsAt = window(primary)
			q.SevenDayUsedPct, q.SevenDayResetsAt = window(secondary)
			// spend_limit is intentionally excluded from allowance fill.
			if q.Fill() < 0 {
				q = nil
			}
		}
	}
	if s.Account == nil {
		warnings = append(warnings, "no fresh account snapshot joined")
	}
	if _, known := s.Account.CreditAvailability(); !known {
		warnings = append(warnings, "credit policy unknown")
	}
	if q == nil {
		warnings = append(warnings, "no fresh pre-call allowance snapshot")
	}
	if len(s.Limits) > 0 {
		warnings = append(warnings, "failure categories have unknown budget scope")
	}
	return s, q, warnings
}

// Several rollout observations can be delivered by the same hook with identical
// observation times. Use their source ordering, independent of export row order.
func laterEvidence(a, b *Event) bool {
	if !a.Time.Equal(b.Time) {
		return a.Time.After(b.Time)
	}
	stream := str(a.Attrs, "source_stream")
	if stream == "" || stream != str(b.Attrs, "source_stream") {
		return false
	}
	for _, key := range []string{"source_offset", "observation_sequence"} {
		x, y := number(a.Attrs, key), number(b.Attrs, key)
		if x != nil && y != nil {
			return *x > *y
		}
	}
	return false
}
