package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// queueAsAnOlderTerma appends an event to the queue an older terma's hooks write, as one
// still running does: with no account.
func queueAsAnOlderTerma(t *testing.T, name string) {
	t.Helper()
	b, err := json.Marshal(spool.Event{Name: name, Time: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(testApp.stateDir, spool.Dir, spool.LegacyEventsFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

// writeOlderSignIns leaves credentials.json as an older terma signed in as users would.
func writeOlderSignIns(t *testing.T, authURL string, users ...string) {
	t.Helper()
	orgs := map[string]any{}
	for i, u := range users {
		org := "org-" + string(rune('a'+i))
		orgs[org] = map[string]any{"session_id": "old-" + org, "auth_url": authURL, "organization_id": org, "user_id": u}
	}
	data, err := json.Marshal(map[string]any{"default": map[string]any{"organizations": orgs}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.LegacyCredentialsPath(testApp.dir), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// queued is the new queue's events by name.
func queued(t *testing.T, s *spool.Spool) map[string]spool.Event {
	t.Helper()
	events, err := s.Peek(100)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]spool.Event{}
	for _, e := range events {
		out[e.Name] = e
	}
	return out
}

// A person's sign-in that retires older sign-ins naming them alone decides their backlog is
// theirs: the older queue's lines go to that person's lane, once, and the relay takes an
// older relay's parts into it too, once. What an older terma still running queues after
// is no one sign-in's: a sign-in since may be someone else's.
func TestAPersonsRetirementTakesTheOlderQueueIntoTheirLane(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	writeOlderSignIns(t, f.srv.URL, "u-1", "u-1")
	s := spoolForTest(t)
	queueAsAnOlderTerma(t, "before")
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := testApp.storeLogin(context.Background(), cfg, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	want := auth.AccountKey(f.srv.URL, "u-1")
	testApp.ingestLegacySpool(s, false)
	if got := queued(t, s)["before"]; got.Account != want || got.NoPerson {
		t.Fatalf("an older terma's backlog = %+v; want it in %s's lane", got, want)
	}
	queueAsAnOlderTerma(t, "after")
	// The flush takes in what the older terma queued since; with no team id, it is
	// counted unroutable once taken.
	res, err := testApp.flushSpool(context.Background(), true, 0)
	if err != nil || res.Unroutable != 2 {
		t.Fatalf("flush = %+v, %v; want both older events taken in", res, err)
	}
	if _, err := os.Stat(filepath.Join(testApp.stateDir, spool.Dir, spool.LegacyEventsFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the older queue outlived the flush: %v", err)
	}
	queueAsAnOlderTerma(t, "later")
	testApp.ingestLegacySpool(s, false)
	if got := queued(t, s)["later"]; got.Account != "" || got.NoPerson {
		t.Fatalf("an older terma's later event = %+v; want it ambiguous", got)
	}
	var engine relay.Options
	testApp.bindRelay(&engine, cfg)
	if got := engine.LegacyOwner(); got != want {
		t.Fatalf("the relay takes an older relay's parts in as %q, want %q", got, want)
	}
}

// The decision sets the older queues aside as it is taken, at sign-in: what an older terma
// queues after, before any flush or relay sweep, is ambiguous, never the decision's.
func TestTheDecisionSetsTheOlderQueuesAsideAtSignIn(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	writeOlderSignIns(t, f.srv.URL, "u-1")
	s := spoolForTest(t)
	queueAsAnOlderTerma(t, "before")
	outbox := filepath.Join(claim.Dir(testApp.stateDir), relay.LegacyOutboxDir)
	if err := os.MkdirAll(filepath.Join(outbox, "p1"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := testApp.storeLogin(context.Background(), cfg, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outbox); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the older outbox was not set aside at sign-in: %v", err)
	}
	queueAsAnOlderTerma(t, "between")
	testApp.ingestLegacySpool(s, false)
	got := queued(t, s)
	if want := auth.AccountKey(f.srv.URL, "u-1"); got["before"].Account != want || got["between"].Account != "" || got["between"].NoPerson {
		t.Fatalf("queued %+v; want before under the decision, and between ambiguous", got)
	}
}

// Older sign-ins of several people are ambiguous: their backlog is taken in untagged, which
// no sign-in nor key sends, and ages out counted apart.
func TestSeveralPeoplesBacklogIsAmbiguous(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	writeOlderSignIns(t, f.srv.URL, "u-1", "u-2")
	s := spoolForTest(t)
	queueAsAnOlderTerma(t, "theirs")
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := testApp.storeLogin(context.Background(), cfg, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	testApp.ingestLegacySpool(s, false)
	if got, ok := queued(t, s)["theirs"]; !ok || got.Account != "" || got.NoPerson {
		t.Fatalf("taken in as %+v (%v); want it untagged", got, ok)
	}
}

// On a server-key machine, the older terma's backlog is the key's, decided at the first
// look even with its sign-ins still on the machine.
func TestAServerKeyMachinesBacklogIsNoPersons(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	serverKeyProfile(t)
	writeOlderSignIns(t, f.srv.URL, "u-1")
	s := spoolForTest(t)
	queueAsAnOlderTerma(t, "keys")
	testApp.ingestLegacySpool(s, true)
	if got, ok := queued(t, s)["keys"]; !ok || !got.NoPerson || got.Account != "" {
		t.Fatalf("taken in as %+v (%v); want it no person's", got, ok)
	}
}

// With no decision yet, the older sign-ins still on a person's machine and none retired,
// nothing is taken in: the older queue stays as it is.
func TestNoDecisionTakesNothingIn(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	writeOlderSignIns(t, f.srv.URL, "u-1")
	s := spoolForTest(t)
	queueAsAnOlderTerma(t, "waiting")
	if _, err := testApp.flushSpool(context.Background(), true, 0); err != nil {
		t.Fatal(err)
	}
	if n, _, err := s.Pending(); err != nil || n != 0 {
		t.Fatalf("pending = %d, %v; want nothing taken in", n, err)
	}
	data, err := os.ReadFile(filepath.Join(testApp.stateDir, spool.Dir, spool.LegacyEventsFile))
	if err != nil || len(data) == 0 {
		t.Fatalf("the older queue = %q, %v; want it left as it was", data, err)
	}
}

// An older terma that signed out before the upgrade left its backlog and no sign-ins: a
// person's sign-in records it ambiguous all the same, so it is taken in untagged and ages
// out counted, whether it was in the spool or the relay's outbox. With nothing queued,
// nothing is recorded.
func TestABacklogWithNoOlderSignInsIsAmbiguous(t *testing.T) {
	for _, where := range []string{"spool", "outbox", "none"} {
		t.Run(where, func(t *testing.T) {
			f := newFakeAuth(t)
			authSandbox(t, f)
			s := spoolForTest(t)
			switch where {
			case "spool":
				queueAsAnOlderTerma(t, "signed-out")
			case "outbox":
				route := filepath.Join(claim.Dir(testApp.stateDir), relay.LegacyOutboxDir, "770e8400-e29b-41d4-a716-446655440000", "_", "_")
				if err := os.MkdirAll(route, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(route, "00000000000000000001-000001-logs-1.pb"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := testApp.loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if err := testApp.storeLogin(context.Background(), cfg, storedSession(f)); err != nil {
				t.Fatal(err)
			}
			rec, err := auth.ReadLegacyRecord(testApp.stateDir)
			if want := map[bool]string{true: "", false: auth.LegacyAccountAmbiguous}[where == "none"]; err != nil || rec.Account != want {
				t.Fatalf("record = %+v, %v; want %q", rec, err, want)
			}
			if where != "spool" {
				return
			}
			testApp.ingestLegacySpool(s, false)
			if got, ok := queued(t, s)["signed-out"]; !ok || got.Account != "" || got.NoPerson {
				t.Fatalf("taken in as %+v (%v); want it untagged", got, ok)
			}
		})
	}
}

// The decision is taken from the same read as the sign-ins it names: one read that names a
// single person whole decides for them, whatever a second look at the file would say.
func TestTheDecisionComesFromOneRead(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	// What is on disk doesn't parse: a second read would name no one.
	if err := os.WriteFile(config.LegacyCredentialsPath(testApp.dir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := testApp.legacySignIns
	t.Cleanup(func() { testApp.legacySignIns = read })
	testApp.legacySignIns = func(string, bool) ([]auth.LegacySignIn, bool, error) {
		return []auth.LegacySignIn{{SessionID: "old", AuthURL: f.srv.URL, UserID: "u-1"}}, true, nil
	}
	if err := testApp.retireLegacySessions(context.Background(), f.srv.URL, false); err != nil {
		t.Fatal(err)
	}
	if rec, err := auth.ReadLegacyRecord(testApp.stateDir); err != nil || rec.Account != auth.AccountKey(f.srv.URL, "u-1") {
		t.Fatalf("record = %+v, %v; want the one read's person", rec, err)
	}
}

// A decision the record holds that is neither a person's key nor a lane is ambiguous, as
// the relay takes it: the older queue is taken in untagged, never under that string.
func TestAnUnknownDecisionIsAmbiguous(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	s := spoolForTest(t)
	if err := config.WriteJSON(filepath.Join(testApp.stateDir, auth.LegacyRecordFile), auth.LegacyRecord{Account: "someone", RetiredAt: time.Now()}, 0o600); err != nil {
		t.Fatal(err)
	}
	queueAsAnOlderTerma(t, "theirs")
	testApp.ingestLegacySpool(s, false)
	if got, ok := queued(t, s)["theirs"]; !ok || got.Account != "" || got.NoPerson {
		t.Fatalf("taken in as %+v (%v); want it untagged", got, ok)
	}
}

// An older terma's flush holding its queue's lock through a sign-in leaves the older queue
// where it is, and so no decision and the older sign-ins kept: a decision taken without its
// queue set aside would take in what it queues later. The next sign-in decides, and sets
// aside what the older terma queued meanwhile too, which came before the decision.
func TestASignInDuringAnOlderFlushDecidesNothing(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	writeOlderSignIns(t, f.srv.URL, "u-1")
	s := spoolForTest(t)
	queueAsAnOlderTerma(t, "before")
	release, err := flock.Lock(context.Background(), filepath.Join(testApp.stateDir, spool.Dir, "delivery.lock"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := testApp.storeLogin(context.Background(), cfg, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	if rec, err := auth.ReadLegacyRecord(testApp.stateDir); err != nil || rec.Account != "" {
		t.Fatalf("record = %+v, %v; want no decision while the older flush held its queue", rec, err)
	}
	if !auth.HasLegacySignIns(testApp.dir) {
		t.Fatal("the older sign-ins went with no decision kept")
	}
	release()
	queueAsAnOlderTerma(t, "between")
	if err := testApp.storeLogin(context.Background(), cfg, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	testApp.ingestLegacySpool(s, false)
	want := auth.AccountKey(f.srv.URL, "u-1")
	if got := queued(t, s); got["before"].Account != want || got["between"].Account != want {
		t.Fatalf("queued %+v; want both under the decision taken after them", got)
	}
}

// What a sign-in set aside before it failed to decide is still an older terma's backlog
// with its sign-ins gone: the next sign-in decides, and takes it in under the decision.
func TestASetAsideBacklogWithNoDecisionIsDecidedLater(t *testing.T) {
	for _, queue := range []string{auth.LegacySpool, auth.LegacyOutbox} {
		t.Run(queue, func(t *testing.T) {
			f := newFakeAuth(t)
			authSandbox(t, f)
			s := spoolForTest(t)
			outbox := filepath.Join(claim.Dir(testApp.stateDir), relay.LegacyOutboxDir)
			if queue == auth.LegacySpool {
				queueAsAnOlderTerma(t, "before")
				if err := s.StageLegacy(); err != nil {
					t.Fatal(err)
				}
			} else {
				route := filepath.Join(outbox, "770e8400-e29b-41d4-a716-446655440000", "_", "_")
				if err := os.MkdirAll(route, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(route, "00000000000000000001-000001-logs-1.pb"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := relay.StageLegacyOutbox(outbox); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := testApp.loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if err := testApp.storeLogin(context.Background(), cfg, storedSession(f)); err != nil {
				t.Fatal(err)
			}
			if rec, err := auth.ReadLegacyRecord(testApp.stateDir); err != nil || rec.Account != auth.LegacyAccountAmbiguous {
				t.Fatalf("record = %+v, %v; want the set-aside backlog decided (no one's sign-ins left: ambiguous)", rec, err)
			}
			if queue == auth.LegacySpool {
				testApp.ingestLegacySpool(s, false)
				if got, ok := queued(t, s)["before"]; !ok || got.Account != "" || got.NoPerson {
					t.Fatalf("queued %+v; want the set-aside event taken in under the decision", queued(t, s))
				}
			}
		})
	}
}
