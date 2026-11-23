package spool

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
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
// (IngestLegacy), never through an append. "" when e is tagged. bound is that the append
// gave e the lane: one that didn't (AppendAsCollected) replays what was recorded with no
// lane, which only an older terma records.
func untagged(e Event, lane Lane, bound bool) string {
	switch {
	case e.Account != "" || e.NoPerson:
		return ""
	case bound && lane.Lost != "":
		return lane.Lost
	}
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
	f, err := os.OpenFile(filepath.Join(dir, now.Format(time.DateOnly)+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return err
	}
	_, err = f.Write(EncodeLoss(reason, n, now))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	pruneLosses(dir, now)
	return err
}

// EncodeLoss is the line CountLost appends for n events lost for reason at at: a hook that
// can't write the spool keeps it elsewhere, for ImportLosses to take in.
func EncodeLoss(reason string, n int, at time.Time) []byte {
	line, _ := json.Marshal(lossLine{Reason: reason, Count: n, Time: at.UTC()})
	return append(line, '\n')
}

// ImportLosses moves the file at path, of lines EncodeLoss made, into the losses as a file
// of its own named for id, which its caller keeps for that file alone: moved again, after a
// crash between the copy and the removal, it lands on its own copy, so it is counted once.
func (s *Spool) ImportLosses(path, id string) error {
	dir := filepath.Join(s.dir, lossesDir)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	dst := filepath.Join(dir, importedPrefix+id+".jsonl")
	if os.Rename(path, dst) == nil {
		return nil
	}
	// Another filesystem: copied whole, then removed.
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(dst, data, fileMode); err != nil {
		return err
	}
	return os.Remove(path)
}

// importedPrefix starts the name of a file of losses ImportLosses took in.
const importedPrefix = "imported~"

// pruneLosses deletes the files past LossWindow, which no hook appends to any more: a day
// by its name, an imported file by when it was last written.
func pruneLosses(dir string, now time.Time) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		day, err := time.Parse(time.DateOnly, strings.TrimSuffix(e.Name(), ".jsonl"))
		if info, ierr := e.Info(); err != nil && ierr == nil && strings.HasPrefix(e.Name(), importedPrefix) {
			day, err = info.ModTime(), nil
		}
		if err == nil && now.Sub(day) > LossWindow+24*time.Hour {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// lossesReportedFile, in the losses folder, is how far TakeNewLosses has read each file.
const lossesReportedFile = "reported.json"

// TakeNewLosses counts, by reason, what hooks lost into the spool directory dir since it was
// last called, and keeps how far it read each file, so each loss is handed out once: the
// relay adds them to its heartbeat's counters. How far it read is kept before the counts are
// handed out, so a crash between the two loses them, never counts them twice. A line not yet
// whole waits for the next call.
func TakeNewLosses(dir string) (map[string]int, error) {
	ldir := filepath.Join(dir, lossesDir)
	read := map[string]int64{}
	if data, err := os.ReadFile(filepath.Join(ldir, lossesReportedFile)); err == nil {
		_ = json.Unmarshal(data, &read) // written atomically; one that won't decode is read as none
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	entries, err := os.ReadDir(ldir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	counts, next := map[string]int{}, map[string]int64{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(ldir, e.Name()))
		if err != nil {
			return nil, err
		}
		from := min(read[e.Name()], int64(len(data)))
		whole := from + int64(bytes.LastIndexByte(data[from:], '\n')+1)
		for line := range bytes.Lines(data[from:whole]) {
			var l lossLine
			if json.Unmarshal(line, &l) == nil && l.Reason != "" && l.Count > 0 {
				counts[l.Reason] += l.Count
			}
		}
		next[e.Name()] = whole
	}
	if err := config.WriteJSON(filepath.Join(ldir, lossesReportedFile), next, fileMode); err != nil {
		return nil, err
	}
	return counts, nil
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
