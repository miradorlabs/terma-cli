package sendas

import (
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

// The otel gateway's refusal of a user token is a google.rpc.Status, protobuf for the
// relay's protobuf requests and JSON otherwise; its ErrorInfo's reason is read from both.
func TestStatusReason(t *testing.T) {
	t.Parallel()
	info := protowire.AppendTag(nil, 1, protowire.BytesType)
	info = protowire.AppendString(info, "SIGN_IN_RETIRED")
	anyMsg := protowire.AppendTag(nil, 1, protowire.BytesType)
	anyMsg = protowire.AppendString(anyMsg, "type.googleapis.com/google.rpc.ErrorInfo")
	anyMsg = protowire.AppendTag(anyMsg, 2, protowire.BytesType)
	anyMsg = protowire.AppendBytes(anyMsg, info)
	status := protowire.AppendTag(nil, 1, protowire.VarintType)
	status = protowire.AppendVarint(status, 7)
	status = protowire.AppendTag(status, 3, protowire.BytesType)
	status = protowire.AppendBytes(status, anyMsg)
	if got := Reason(status); got != "SIGN_IN_RETIRED" {
		t.Fatalf("protobuf: %q", got)
	}
	jsonStatus := []byte(`{"code":7,"message":"m","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"ORG_NOT_ACCESSIBLE","domain":"gateway"}]}`)
	if got := Reason(jsonStatus); got != "ORG_NOT_ACCESSIBLE" {
		t.Fatalf("JSON: %q", got)
	}
	if got := Reason([]byte(`{"code":"x","message":"an API key's refusal"}`)); got != "" {
		t.Fatalf("no reason: %q", got)
	}
}
