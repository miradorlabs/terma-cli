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

// WireFields reads each field in order with its value, and stops at the first it can't
// read; WireBytes is the first length-delimited field of a number.
func TestWireFields(t *testing.T) {
	t.Parallel()
	msg := protowire.AppendTag(nil, 1, protowire.VarintType)
	msg = protowire.AppendVarint(msg, 300)
	msg = protowire.AppendTag(msg, 2, protowire.Fixed32Type)
	msg = protowire.AppendFixed32(msg, 9)
	msg = protowire.AppendTag(msg, 3, protowire.BytesType)
	msg = protowire.AppendString(msg, "first")
	msg = protowire.AppendTag(msg, 3, protowire.BytesType)
	msg = protowire.AppendString(msg, "second")
	var got []WireField
	for f := range WireFields(msg) {
		got = append(got, f)
	}
	if len(got) != 4 || got[0].Varint != 300 || got[1].Type != protowire.Fixed32Type || string(got[2].Bytes) != "first" || string(got[3].Bytes) != "second" {
		t.Fatalf("fields = %+v", got)
	}
	if b := WireBytes(msg, 3); string(b) != "first" {
		t.Fatalf("WireBytes(3) = %q, want the first", b)
	}
	if b := WireBytes(msg, 4); b != nil {
		t.Fatalf("WireBytes(4) = %q, want none", b)
	}
	// A field cut short ends the walk: the fields before it are read, it and what follows not.
	cut := msg[:len(msg)-1]
	n := 0
	for range WireFields(cut) {
		n++
	}
	if n != 3 {
		t.Fatalf("read %d fields of a message cut short in its fourth, want 3", n)
	}
	if b := WireBytes(cut[len(msg)-len("second")-2:], 3); b != nil {
		t.Fatalf("WireBytes read %q from a field cut short", b)
	}
	// A walk stopped early stops, and the same sequence walked again starts over.
	seq := WireFields(msg)
	for f := range seq {
		if f.Num != 1 {
			t.Fatalf("walked on to field %d after a break", f.Num)
		}
		break
	}
	for range 2 {
		var again []WireField
		for f := range seq {
			again = append(again, f)
		}
		if len(again) != 4 || again[0].Varint != 300 || string(again[3].Bytes) != "second" {
			t.Fatalf("a sequence walked again read %+v, want the four fields from the start", again)
		}
	}
}
