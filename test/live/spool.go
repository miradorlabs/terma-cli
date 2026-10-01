package live

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Event is one spooled hook event, as `terma hook` wrote it.
type Event struct {
	Time      time.Time      `json:"time"`
	Name      string         `json:"name"`
	SessionID string         `json:"session_id"`
	Repo      string         `json:"repo"`
	Attrs     map[string]any `json:"attrs"`
}

// Spool reads every event the sandbox's hooks have written.
func (sb *Sandbox) Spool() []Event {
	f, err := os.Open(filepath.Join(sb.TermaConfig, "spool", "events.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// Events filters what the hooks produced by name and, when given, session:
// what is still in the spool file plus what a background flush has already
// delivered to the receiver. The end-of-turn hooks flush within seconds, so
// an event is usually found on the receiver side, in the shape the backend
// parses, with attribute values as strings.
func (sb *Sandbox) Events(name, session string) []Event {
	var out []Event
	for _, e := range sb.Spool() {
		if e.Name == name && (session == "" || e.SessionID == session) {
			out = append(out, e)
		}
	}
	for _, l := range sb.Receiver.Logs() {
		if l.Resource["service.name"] != "terma-cli" || l.Attrs["event.name"] != name {
			continue
		}
		if session != "" && l.Attrs["session.id"] != session {
			continue
		}
		e := Event{Time: l.Time, Name: name, SessionID: l.Attrs["session.id"], Repo: l.Attrs["terma.repo"], Attrs: map[string]any{}}
		for k, v := range l.Attrs {
			switch k {
			case "event.name", "session.id", "terma.repo":
				continue
			}
			e.Attrs[k] = v
		}
		out = append(out, e)
	}
	// The spool and receiver are separate streams; their concatenation is not
	// chronological. Contracts asking for the last snapshot need event time.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

// WaitEvents polls the spool for an event of that name and session.
func (sb *Sandbox) WaitEvents(name, session string, timeout time.Duration) []Event {
	deadline := time.Now().Add(timeout)
	for {
		if evs := sb.Events(name, session); len(evs) > 0 || time.Now().After(deadline) {
			return evs
		}
		time.Sleep(200 * time.Millisecond)
	}
}
