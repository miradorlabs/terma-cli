package spool

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// Lane is whose what a hook collects is: a person's account key (Account), no person's
// under a server key (NoPerson), or, with neither, no one's, and Lost says why it is lost.
type Lane struct {
	Account  string
	NoPerson bool
	Lost     string
}

// LaneOf is the lane of what is collected under account, as auth.Account names the
// sign-in in place, or under a server key (serverKey): signed out ("") and a sign-in that
// can't be read (sendas.AccountUnreadable) name no one, so what they collect is lost.
func LaneOf(account string, serverKey bool) Lane {
	switch {
	case serverKey:
		return Lane{NoPerson: true}
	case account == "":
		return Lane{Lost: sendas.LostSignedOut}
	case account == sendas.AccountUnreadable:
		return Lane{Lost: sendas.LostUnreadable}
	}
	return Lane{Account: account}
}

// untagged is why e, about to be queued in lane, can't be: it names no account and no
// person, so no one could send it. An older terma's backlog is the only such kind queued
// (IngestLegacy), never through an append. "" when e is tagged.
func untagged(e Event, lane Lane) string {
	switch {
	case e.Account == sendas.AccountUnreadable:
		return sendas.LostUnreadable
	case e.Account != "" || e.NoPerson:
		return ""
	case lane.Lost != "":
		return lane.Lost
	}
	// Replayed untagged into a bound spool: what an older terma parked, whose no one
	// sign-in may claim.
	return sendas.LostLegacyAmbiguous
}

// lossesDir, in Dir, keeps what hooks lost: a file per UTC day of one JSON line per loss
// (lossLine), each written whole in one append, so concurrent hooks never lose a count,
// and a day past LossWindow is deleted whole.
const lossesDir = "losses"

// LossWindow is how long a hook's loss is counted: as long as the spool keeps an event.
const LossWindow = MaxAge

// lossLine is one hook's loss of Count events, for Reason.
type lossLine struct {
	Reason string    `json:"reason"`
	Count  int       `json:"count"`
	Time   time.Time `json:"time"`
}

// CountLost records that n events were lost for reason, as one line appended to today's
// file: never read and rewritten, so no hook's count overwrites another's.
func (s *Spool) CountLost(reason string, n int) error {
	if n <= 0 {
		return nil
	}
	dir := filepath.Join(s.dir, lossesDir)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	now := time.Now().UTC()
	line, err := json.Marshal(lossLine{Reason: reason, Count: n, Time: now})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, now.Format(time.DateOnly)+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	pruneLosses(dir, now)
	return err
}

// pruneLosses deletes the days past LossWindow, which no hook appends to any more.
func pruneLosses(dir string, now time.Time) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		day, err := time.Parse(time.DateOnly, strings.TrimSuffix(e.Name(), ".jsonl"))
		if err == nil && now.Sub(day) > LossWindow+24*time.Hour {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// Losses are what hooks lost within LossWindow of now, by reason, and when the last was.
type Losses struct {
	Counts map[string]int
	Last   time.Time
}

// ReadLosses reads what hooks lost into the spool directory dir within LossWindow of now.
// A line not yet whole, or torn, is skipped.
func ReadLosses(dir string, now time.Time) (Losses, error) {
	l := Losses{Counts: map[string]int{}}
	entries, err := os.ReadDir(filepath.Join(dir, lossesDir))
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, lossesDir, e.Name()))
		if err != nil {
			return l, err
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var line lossLine
			if json.Unmarshal(sc.Bytes(), &line) != nil || line.Reason == "" || line.Count <= 0 || now.Sub(line.Time) > LossWindow {
				continue
			}
			l.Counts[line.Reason] += line.Count
			if line.Time.After(l.Last) {
				l.Last = line.Time
			}
		}
		f.Close()
	}
	return l, nil
}
