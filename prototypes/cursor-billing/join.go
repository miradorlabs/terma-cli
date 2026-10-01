package cursorbilling

import (
	"fmt"
	"math/big"
)

// Session is a known conversation from hook telemetry. The calling platform must
// supply matching tenant/connection scope, and may supply the observed account.
// There is no inferred allocation from session cost to individual generations.
type Session struct {
	ConversationID string   `json:"conversation_id"`
	UserEmail      string   `json:"user_email,omitempty"`
	TurnIDs        []string `json:"turn_ids,omitempty"`
}

// Amount shows a known subtotal plus how many records lacked the field. A zero
// subtotal with missing records is not proof that usage was free.
type Amount struct {
	KnownCents    Decimal `json:"known_cents"`
	MissingEvents int     `json:"missing_events"`
}

func (a *Amount) add(d *Decimal) error {
	if a.KnownCents == "" {
		a.KnownCents = "0"
	}
	if d == nil {
		a.MissingEvents++
		return nil
	}
	r, err := d.rat()
	if err != nil {
		return err
	}
	total, err := a.KnownCents.rat()
	if err != nil {
		return err
	}
	a.KnownCents = exactDecimal(new(big.Rat).Add(total, r))
	return nil
}

type SessionCost struct {
	Session            Session        `json:"session"`
	MatchedEvents      int            `json:"matched_events"`
	IdentityMismatches int            `json:"identity_mismatches"`
	ModelReference     Amount         `json:"model_reference"`
	ReportedCharge     Amount         `json:"reported_charge"`
	CursorFee          Amount         `json:"cursor_fee"`
	BillingCategories  map[string]int `json:"billing_categories"`
	Status             string         `json:"status"`
}
type JoinResult struct {
	Scope           Scope         `json:"scope"`
	Window          Window        `json:"window"`
	ContentID       string        `json:"content_id"`
	Sessions        []SessionCost `json:"sessions"`
	UnmatchedEvents int           `json:"unmatched_events"`
	Warnings        []string      `json:"warnings"`
}

func JoinSessions(snapshot UsageSnapshot, scope Scope, sessions []Session) (JoinResult, error) {
	out := JoinResult{Scope: scope, Window: snapshot.Window, ContentID: snapshot.ContentID, Sessions: []SessionCost{}, Warnings: []string{
		"Costs are joined at conversation scope only; no per-turn allocation is implied.",
		"Reported charge is not necessarily extra cash paid: preserve included/on-demand billing categories.",
		"No matching events can mean reporting lag or missing coverage; it does not establish zero cost.",
	}}
	if err := scope.validate(); err != nil {
		return JoinResult{}, err
	}
	if snapshot.Scope != scope || !snapshot.Complete {
		return JoinResult{}, fmt.Errorf("session join needs a complete snapshot with matching tenant/connection scope")
	}
	index := map[string]int{}
	for _, s := range sessions {
		if s.ConversationID == "" {
			return JoinResult{}, fmt.Errorf("conversation ID required")
		}
		if _, ok := index[s.ConversationID]; ok {
			return JoinResult{}, fmt.Errorf("duplicate session identity")
		}
		index[s.ConversationID] = len(out.Sessions)
		out.Sessions = append(out.Sessions, SessionCost{Session: s, ModelReference: Amount{KnownCents: "0"}, ReportedCharge: Amount{KnownCents: "0"}, CursorFee: Amount{KnownCents: "0"}, BillingCategories: map[string]int{}, Status: "not_observed"})
	}
	for _, e := range snapshot.Events {
		i, ok := index[e.ConversationID]
		if !ok {
			out.UnmatchedEvents++
			continue
		}
		row := &out.Sessions[i]
		if row.Session.UserEmail != "" && e.UserEmail != row.Session.UserEmail {
			row.IdentityMismatches++
			out.UnmatchedEvents++
			continue
		}
		row.MatchedEvents++
		row.Status = "matched"
		row.BillingCategories[e.Kind]++
		var model *Decimal
		if e.TokenUsage != nil {
			model = e.TokenUsage.TotalCents
		}
		for _, pair := range []struct {
			a *Amount
			d *Decimal
		}{{&row.ModelReference, model}, {&row.ReportedCharge, e.ChargedCents}, {&row.CursorFee, e.CursorTokenFee}} {
			if err := pair.a.add(pair.d); err != nil {
				return JoinResult{}, fmt.Errorf("invalid amount in snapshot")
			}
		}
	}
	for i := range out.Sessions {
		if out.Sessions[i].IdentityMismatches > 0 {
			out.Sessions[i].Status = "identity_mismatch"
		}
	}
	return out, nil
}
