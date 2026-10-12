package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
)

// `terma relay classify` answers as the relay's content policy treats each key, with the
// agents this terma knows: the e2e field census classifies every key it saw with it.
func TestRelayClassify(t *testing.T) {
	c := testApp.newRelayCommand()
	var out bytes.Buffer
	c.SetArgs([]string{"classify"})
	c.SetIn(strings.NewReader(`[{"site": "record", "key": "from_mode"}, {"site": "record", "key": "prompt"},
		{"site": "record", "key": "a.key.no.rule.names"}, {"site": "resource", "key": "a.key.no.rule.names"},
		{"site": "event", "event": "tool.output", "key": "content"}]`))
	c.SetOut(&out)
	c.SetErr(io.Discard)
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []struct {
		Class string   `json:"class"`
		Kept  []string `json:"kept"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	var said []string
	for _, v := range got {
		said = append(said, v.Class+" "+strings.Join(v.Kept, ","))
	}
	want := []string{"safe text,number,bool,list,map,bytes", "prompt ", "unclassified number,bool", "unclassified ", "tool_content "}
	if !slices.Equal(said, want) {
		t.Errorf("classify = %q, want %q", said, want)
	}
	for _, in := range []string{"not json", `[{"site": "nowhere", "key": "k"}]`} {
		c = testApp.newRelayCommand()
		c.SetArgs([]string{"classify"})
		c.SetIn(strings.NewReader(in))
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		if err := c.ExecuteContext(context.Background()); err == nil {
			t.Errorf("classify accepted %s", in)
		}
	}
}
