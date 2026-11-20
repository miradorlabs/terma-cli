package relay

import (
	"encoding/json"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

// reasonSignInRetired is the reason a gateway gives a sign-in it no longer accepts.
const reasonSignInRetired = "SIGN_IN_RETIRED"

// errorInfoType ends the type URL of a google.rpc.ErrorInfo detail.
const errorInfoType = "google.rpc.ErrorInfo"

// statusReason reads the reason of the google.rpc.ErrorInfo in a refusal's body: the
// whole google.rpc.Status the otel gateway answers a user token with, as protobuf for the
// relay's protobuf requests, or as JSON. "" when it carries none. Read by hand, as
// partialSuccess is, to keep gRPC's packages out.
func statusReason(body []byte) string {
	if reason := jsonStatusReason(body); reason != "" {
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
			if reason := anyErrorInfoReason(detail); reason != "" {
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

// anyErrorInfoReason reads an Any's reason when it holds an ErrorInfo.
func anyErrorInfoReason(anyMsg []byte) string {
	typeURL := protoField(anyMsg, 1, protowire.BytesType)
	value := protoField(anyMsg, 2, protowire.BytesType)
	if !strings.HasSuffix(string(typeURL), errorInfoType) {
		return ""
	}
	return string(protoField(value, 1, protowire.BytesType))
}

func jsonStatusReason(body []byte) string {
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
