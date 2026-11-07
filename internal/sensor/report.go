package sensor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay"
)

// ReportEvery is the window of the rollups the sensor sends the relay.
var ReportEvery = time.Minute

// snap is a session's running totals at one moment.
type snap struct {
	net                map[string]Traffic
	read, written      uint64
	removed, procs     int
	blocks, containers int
	end                time.Time
}

// snapshot is every session's running totals now, open connections and files included.
func (s *State) snapshot() map[int]snap {
	live, files := s.backend.Live(), s.backend.LiveFiles()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int]snap, len(s.sessions))
	for root, ss := range s.sessions {
		sn := snap{net: map[string]Traffic{}, procs: ss.Procs, blocks: len(ss.Blocks), containers: len(ss.Containers), end: ss.End}
		for h, t := range ss.Net {
			sn.net[h] = *t
		}
		for _, f := range live {
			if f.Root == root {
				h := s.hostLocked(f)
				t := sn.net[h]
				t.TX, t.RX, t.Flows = t.TX+f.TX, t.RX+f.RX, t.Flows+1
				sn.net[h] = t
			}
		}
		for _, u := range ss.Files {
			sn.read, sn.written = sn.read+u.Read, sn.written+u.Written
			if u.Removed {
				sn.removed++
			}
		}
		for _, f := range files {
			if f.Root == root {
				sn.read, sn.written = sn.read+f.Read, sn.written+f.Written
			}
		}
		out[root] = sn
	}
	return out
}

// Report sends the relay at url each session's rollup every ReportEvery: what moved since
// the last rollup the relay placed. A rollup the relay hands back (its root has not named
// a session yet) stays here and goes with the next window; a session that ended is
// dropped once its last window is placed.
func Report(ctx context.Context, st *State, url, token string, log *slog.Logger) {
	sent := map[int]snap{}
	last := time.Now()
	t := time.NewTicker(ReportEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			cur := st.snapshot()
			rollups := make([]relay.SensorRollup, 0, len(cur))
			for root, c := range cur {
				if r, ok := st.rollup(root, c, sent[root], last, now); ok {
					rollups = append(rollups, r)
				}
			}
			if len(rollups) == 0 {
				last = now
				continue
			}
			reply, err := post(ctx, url, token, rollups)
			if err != nil {
				log.Warn("sensor report", "err", err)
				continue
			}
			for _, root := range reply.Placed {
				sent[root] = cur[root]
				if !cur[root].end.IsZero() {
					st.forget(root)
					delete(sent, root)
				}
			}
			if len(reply.Unplaced) > 0 {
				log.Info("sensor report: roots not placed yet", "roots", reply.Unplaced)
			}
			last = now
		}
	}
}

// rollup is what moved in root's session between prev and cur, if anything did.
func (s *State) rollup(root int, cur, prev snap, from, to time.Time) (relay.SensorRollup, bool) {
	r := relay.SensorRollup{Root: root, End: to, Window: int(to.Sub(from).Seconds())}
	for h, t := range cur.net {
		p := prev.net[h]
		if d := (relay.SensorTraffic{Host: h, Sent: int64(t.TX - min(p.TX, t.TX)), Received: int64(t.RX - min(p.RX, t.RX)),
			Connections: max(t.Flows-p.Flows, 0)}); d.Sent+d.Received > 0 || d.Connections > 0 {
			r.Network = append(r.Network, d)
		}
	}
	r.FilesRead, r.FilesWrote = int64(cur.read-min(prev.read, cur.read)), int64(cur.written-min(prev.written, cur.written))
	r.Removed, r.Processes = max(cur.removed-prev.removed, 0), max(cur.procs-prev.procs, 0)
	s.mu.Lock()
	if ss := s.sessions[root]; ss != nil {
		for _, b := range ss.Blocks[min(prev.blocks, len(ss.Blocks)):] {
			r.Blocks = append(r.Blocks, relay.SensorBlock{Kind: b.Kind, Rule: b.Rule, Enforced: b.Enforced, At: b.Time})
		}
		for _, c := range ss.Containers[min(prev.containers, len(ss.Containers)):] {
			r.Containers = append(r.Containers, relay.SensorContainer{Name: c.Name, Image: c.Image, At: c.Start})
		}
	}
	s.mu.Unlock()
	moved := len(r.Network)+len(r.Blocks)+len(r.Containers)+r.Removed+r.Processes > 0 || r.FilesRead+r.FilesWrote > 0
	return r, moved
}

// forget drops a session that ended and was reported whole.
func (s *State) forget(root int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, root)
}

func post(ctx context.Context, url, token string, rollups []relay.SensorRollup) (relay.SensorReply, error) {
	var reply relay.SensorReply
	body, _ := json.Marshal(rollups)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/sensor", bytes.NewReader(body))
	if err != nil {
		return reply, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return reply, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return reply, fmt.Errorf("relay: %s", resp.Status)
	}
	return reply, json.NewDecoder(resp.Body).Decode(&reply)
}
