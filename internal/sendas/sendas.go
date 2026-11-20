// Package sendas is what the relay and the spool's delivery send telemetry as: the person's
// sign-in, naming the team, and how the otel gateway answers a user token's refusal. It
// holds no state and does no I/O, so both may import it.
package sendas

import (
	"context"
	"encoding/json"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

// ProjectHeader names the team a request with a user token writes to.
const ProjectHeader = "X-Mirador-Project"

// ReasonSignInRetired is the reason a gateway gives a sign-in it no longer accepts.
const ReasonSignInRetired = "SIGN_IN_RETIRED"

// SignIn is the person's sign-in telemetry is sent with, as it stands at each send: the
// developer can sign in, out or as someone else meanwhile.
type SignIn interface {
	// Account is the account key (auth.AccountKey) of the sign-in now, "" with none.
	Account() string
	// Token is a fresh access token of the sign-in whose account key is account: "" with no
	// error when that sign-in cannot send now (signed out, another person's, retired), an
	// error for what a retry may fix.
	Token(ctx context.Context, account string) (string, error)
	// Refused says a server refused token as unknown or expired: rotate it.
	Refused(ctx context.Context, token string)
	// Retired says a server refused token's sign-in as retired (SIGN_IN_RETIRED): the
	// sign-in that sent it, never one stored since.
	Retired(token string)
}

// errorInfoType ends the type URL of a google.rpc.ErrorInfo detail.
const errorInfoType = "google.rpc.ErrorInfo"

// Reason reads the reason of the google.rpc.ErrorInfo in a refusal's body: the whole
// google.rpc.Status the otel gateway answers a user token with, as protobuf for a protobuf
// request, or as JSON. "" when it carries none. Read by hand to keep gRPC's packages out.
func Reason(body []byte) string {
	if reason := jsonReason(body); reason != "" {
		return reason
	}
	for b := body; len(b) > 0; {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return ""
		}
		b = b[n:]
		if num == 3 && typ == protowire.BytesType {
			detail, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return ""
			}
			if reason := errorInfoReason(detail); reason != "" {
				return reason
			}
			b = b[m:]
			continue
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return ""
		}
		b = b[m:]
	}
	return ""
}

// errorInfoReason reads an Any's reason when it holds an ErrorInfo.
func errorInfoReason(anyMsg []byte) string {
	if !strings.HasSuffix(string(field(anyMsg, 1)), errorInfoType) {
		return ""
	}
	return string(field(field(anyMsg, 2), 1))
}

// field is the first length-delimited field num of a message, nil when it has none.
func field(b []byte, num protowire.Number) []byte {
	for len(b) > 0 {
		n, t, l := protowire.ConsumeTag(b)
		if l < 0 {
			return nil
		}
		b = b[l:]
		if n == num && t == protowire.BytesType {
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return nil
			}
			return v
		}
		m := protowire.ConsumeFieldValue(n, t, b)
		if m < 0 {
			return nil
		}
		b = b[m:]
	}
	return nil
}

func jsonReason(body []byte) string {
	var status struct {
		Details []struct {
			Type   string `json:"@type"`
			Reason string `json:"reason"`
		} `json:"details"`
	}
	if json.Unmarshal(body, &status) != nil {
		return ""
	}
	for _, d := range status.Details {
		if strings.HasSuffix(d.Type, errorInfoType) && d.Reason != "" {
			return d.Reason
		}
	}
	return ""
}
