package spool

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

func routed() Event {
	return Event{
		Time:        time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC),
		Name:        "terma.file.edit",
		SessionID:   "s1",
		Repository:  config.Repository{Origin: "github.com/acme/api"},
		Attrs:       map[string]any{"k": "v"},
		Profile:     "default",
		Enrollment:  "enr-acme",
		HoldKey:     "github.com/acme",
		Run:         "run-1",
		OwnLocation: true,
	}
}

// A routed event's destination survives its line in the spool, under these names.
func TestDestinationRoundTrips(t *testing.T) {
	t.Parallel()
	e := routed()
	line, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"profile":"default"`, `"enrollment":"enr-acme"`, `"hold_key":"github.com/acme"`, `"run":"run-1"`, `"own_location":true`} {
		if !strings.Contains(string(line), field) {
			t.Errorf("the line lacks %s: %s", field, line)
		}
	}
	var back Event
	if err := json.Unmarshal(line, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, e) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", back, e)
	}
}

// Today's events carry none of the destination fields, and a line spooled before them
// reads with none, so it is routed by attrs.project_id as it always was.
func TestEventsWithoutADestinationStayAsTheyWere(t *testing.T) {
	t.Parallel()
	line, err := json.Marshal(Event{Name: "session.start", SessionID: "s1", Attrs: map[string]any{"project_id": "p1"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"profile", "enrollment", "hold_key", `"run"`, "own_location"} {
		if strings.Contains(string(line), field) {
			t.Errorf("an unrouted event's line holds %s: %s", field, line)
		}
	}
	var old Event
	if err := json.Unmarshal([]byte(`{"time":"2026-11-12T12:00:00Z","name":"session.start","session_id":"s1","attrs":{"project_id":"p1"}}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Profile != "" || old.Enrollment != "" || old.HoldKey != "" || old.Run != "" || old.OwnLocation || old.Attrs["project_id"] != "p1" {
		t.Fatalf("an older line read as %+v", old)
	}
}

// A held event keeps its destination through the spool: the sender sees it as spooled,
// and again after holding it.
func TestQueuedEventsKeepTheirDestination(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(routed()); err != nil {
		t.Fatal(err)
	}
	var seen []Event
	hold := SenderFunc(func(_ context.Context, events []Event) ([]Event, error) {
		seen = append(seen, events...)
		return events, nil
	})
	s.Flush(context.Background(), hold, FlushOptions{Force: true})
	s.Flush(context.Background(), hold, FlushOptions{Force: true})
	if len(seen) != 2 {
		t.Fatalf("the sender saw %d events, want the held one twice", len(seen))
	}
	for i, e := range seen {
		if !reflect.DeepEqual(e, routed()) {
			t.Errorf("pass %d: %+v", i, e)
		}
	}
}
