// Package cursorbilling imports Cursor team billing data. It has no CLI,
// environment, filesystem, database or scheduler dependency. The platform owns
// secrets, tenant binding, polling and atomic replacement of completed windows.
package cursorbilling

import (
	"encoding/json"
	"fmt"
	"time"
)

// Scope is supplied by the platform after binding the key to a tenant connection.
// Team endpoints do not return a team ID; this binding cannot be verified here.
// Rotate ConnectionID if a replacement key belongs to a different Cursor team.
type Scope struct {
	TenantID     string `json:"tenant_id"`
	ConnectionID string `json:"connection_id"`
}

func (s Scope) validate() error {
	if s.TenantID == "" || s.ConnectionID == "" {
		return fmt.Errorf("tenant_id and connection_id are required")
	}
	return nil
}

// Window is half-open with millisecond precision. The client converts End to the
// provider's inclusive final millisecond, avoiding duplicate boundary events.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

func (w Window) validate() error {
	if w.Start.IsZero() || !w.End.After(w.Start) || w.Start.UnixMilli() < 0 || w.Start.Nanosecond()%1000000 != 0 || w.End.Nanosecond()%1000000 != 0 {
		return fmt.Errorf("window needs nonnegative millisecond timestamps and start < exclusive end")
	}
	if w.End.Sub(w.Start) > 31*24*time.Hour {
		return fmt.Errorf("window exceeds 31 days; partition into smaller fixed windows")
	}
	return nil
}

type Member struct {
	ID        string          `json:"id"`
	Email     string          `json:"email"`
	Name      string          `json:"name"`
	Role      string          `json:"role"`
	IsRemoved *bool           `json:"isRemoved"`
	Raw       json.RawMessage `json:"raw"`
}
type Spend struct {
	UserID                       string          `json:"userId"`
	Email                        string          `json:"email"`
	Role                         string          `json:"role"`
	BillingTier                  string          `json:"billingTier,omitempty"` // Unmapped provider code.
	SpendCents                   *Decimal        `json:"spendCents"`
	IncludedSpendCents           *Decimal        `json:"includedSpendCents"`
	OverallSpendCents            *Decimal        `json:"overallSpendCents"`
	MonthlyLimitDollars          *Decimal        `json:"monthlyLimitDollars"`
	HardLimitOverrideDollars     *Decimal        `json:"hardLimitOverrideDollars"`
	EffectivePerUserLimitDollars *Decimal        `json:"effectivePerUserLimitDollars"`
	APIPercentUsed               *Decimal        `json:"apiPercentUsed"`
	AutoPercentUsed              *Decimal        `json:"autoPercentUsed"`
	TotalPercentUsed             *Decimal        `json:"totalPercentUsed"`
	Raw                          json.RawMessage `json:"raw"`
}
type TokenUsage struct {
	InputTokens        *int64   `json:"inputTokens"`
	OutputTokens       *int64   `json:"outputTokens"`
	CacheReadTokens    *int64   `json:"cacheReadTokens"`
	CacheWriteTokens   *int64   `json:"cacheWriteTokens"`
	TotalCents         *Decimal `json:"totalCents"`
	DiscountPercentOff *Decimal `json:"discountPercentOff"`
}
type UsageEvent struct {
	Timestamp        *Millis     `json:"timestamp"`
	UserEmail        string      `json:"userEmail"`
	ServiceAccountID string      `json:"serviceAccountId,omitempty"`
	ConversationID   string      `json:"conversationId,omitempty"`
	Model            string      `json:"model"`
	Kind             string      `json:"kind"`
	IsChargeable     *bool       `json:"isChargeable"`
	IsTokenBasedCall *bool       `json:"isTokenBasedCall"`
	TokenUsage       *TokenUsage `json:"tokenUsage"`
	ChargedCents     *Decimal    `json:"chargedCents"`
	CursorTokenFee   *Decimal    `json:"cursorTokenFee"`
	RequestsCosts    *Decimal    `json:"requestsCosts"`
	// Fingerprint is a content hash, NOT a provider request ID. Identical records
	// retain multiplicity; the API currently exposes no reliable unique event ID.
	Fingerprint string          `json:"fingerprint"`
	Raw         json.RawMessage `json:"raw"`
}

type MembersSnapshot struct {
	Scope      Scope     `json:"scope"`
	ObservedAt time.Time `json:"observed_at"`
	Members    []Member  `json:"members"`
}
type SpendSnapshot struct {
	Scope      Scope     `json:"scope"`
	StartedAt  time.Time `json:"started_at"`
	ObservedAt time.Time `json:"observed_at"`
	CycleStart Millis    `json:"subscription_cycle_start"`
	Members    []Spend   `json:"members"`
	Pages      int       `json:"pages"`
	// RawPages retain unrecognized provider fields and null/absent distinctions.
	RawPages []json.RawMessage `json:"raw_pages"`
}
type UsageSnapshot struct {
	Scope      Scope        `json:"scope"`
	Window     Window       `json:"window"`
	StartedAt  time.Time    `json:"started_at"`
	ObservedAt time.Time    `json:"observed_at"`
	Events     []UsageEvent `json:"events"`
	Pages      int          `json:"pages"`
	Complete   bool         `json:"complete"`
	// ContentID covers scope, window and the multiset of records. It is stable
	// across page order and repeat imports, including repeated identical events.
	ContentID string   `json:"content_id"`
	Warnings  []string `json:"warnings"`
}
