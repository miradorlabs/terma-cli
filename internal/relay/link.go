package relay

import (
	"maps"
	"slices"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/miradorlabs/terma-cli/internal/relay/shape"
)

// A record a link rule names (shape.Link) waits in memory for its span, ahead of the
// claim's hold and the session's order, so its own span never queues behind it there.
const (
	// linkHold covers an exporter that batches spans every five seconds: the span trailed its
	// record by under five seconds in every pair measured.
	//
	// Accepted risk: the agent was answered when the record arrived, so a relay that crashes
	// loses up to linkHold of waiting records (a stopping relay sends them unstamped), and a
	// waiting record leaves after the records of its session that came later. Upgrade path:
	// write waiting records to the outbox and stamp them there.
	linkHold = 8 * time.Second
	// linkTTL keeps a span's value for a record that arrives after it.
	linkTTL = time.Minute
	// maxLinkWaits and maxLinkValues bound both; when full the oldest goes, a record unstamped.
	maxLinkWaits  = 256
	maxLinkValues = 4096
)

// linkID is one call under one link rule: the rule's index and the call's Key value.
type linkID struct {
	rule int
	key  string
}

type linkValue struct {
	value string
	at    time.Time
}

// linkWait is a record waiting for its span, as a part of its own.
type linkWait struct {
	id linkID
	p  *part
	at time.Time
}

// carveLinks moves each log record a link rule waits on out of its part into a part of its
// own, deletes a part left empty, and returns them; on every record it first removes the
// link stamps, which are the relay's to set, never the agent's.
func (ru *rules) carveLinks(parts map[string]*part) []linkWait {
	if len(ru.links) == 0 {
		return nil
	}
	var waits []linkWait
	for s, p := range parts {
		m, ok := p.msg.(*logspb.LogsData)
		if !ok {
			continue
		}
		for _, rl := range m.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				kept := sl.LogRecords[:0]
				for _, lr := range sl.GetLogRecords() {
					lr.Attributes = slices.DeleteFunc(lr.Attributes, func(kv *commonpb.KeyValue) bool {
						return slices.ContainsFunc(ru.links, func(l shape.Link) bool { return l.To == kv.GetKey() })
					})
					id, ok := ru.linkOf(lr.GetAttributes())
					if !ok || s == "" {
						kept = append(kept, lr)
						continue
					}
					waits = append(waits, linkWait{id: id, p: &part{signal: Logs, session: s, claimed: p.claimed, records: 1,
						msg: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
							Resource:  cloneResource(rl.GetResource()),
							SchemaUrl: rl.GetSchemaUrl(),
							ScopeLogs: []*logspb.ScopeLogs{{Scope: sl.GetScope(), SchemaUrl: sl.GetSchemaUrl(), LogRecords: []*logspb.LogRecord{lr}}},
						}}},
					}})
					p.records--
				}
				sl.LogRecords = kept
			}
		}
		if p.records == 0 {
			delete(parts, s)
		}
	}
	return waits
}

// linkOf is the call a link rule waits on for a record, if any.
func (ru *rules) linkOf(attrs []*commonpb.KeyValue) (linkID, bool) {
	for i, l := range ru.links {
		key := attrString(attrs, l.Key)
		if key == "" {
			continue
		}
		matches := true
		for k, v := range l.Record {
			if attrString(attrs, k) != v {
				matches = false
				break
			}
		}
		if matches {
			return linkID{i, key}, true
		}
	}
	return linkID{}, false
}

// linkSpans keeps the value each linked span carries and returns the records that waited for
// it, stamped, to be routed before the spans: a span that carries no value releases its
// record unstamped.
func (r *Relay) linkSpans(parts map[string]*part) []*part {
	if len(r.rules.links) == 0 {
		return nil
	}
	now := r.opts.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*part
	for _, p := range parts {
		for _, rs := range p.msg.(*tracepb.TracesData).GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, sp := range ss.GetSpans() {
					for i, l := range r.rules.links {
						key := attrString(sp.GetAttributes(), l.Key)
						if sp.GetName() != l.Span || key == "" {
							continue
						}
						id, value := linkID{i, key}, attrString(sp.GetAttributes(), l.From)
						if _, known := r.linkValues[id]; !known && len(r.linkValues) >= maxLinkValues {
							r.evictLinkValueLocked()
						}
						r.linkValues[id] = linkValue{value, now}
						r.linkWaits = slices.DeleteFunc(r.linkWaits, func(w linkWait) bool {
							if w.id != id {
								return false
							}
							out = append(out, r.stampLink(w, value))
							return true
						})
					}
				}
			}
		}
	}
	return out
}

// awaitLinks stamps each record whose span came first and returns it with any record the
// full wait evicted, unstamped, to be routed now; the rest wait.
func (r *Relay) awaitLinks(waits []linkWait) []*part {
	if len(waits) == 0 {
		return nil
	}
	now := r.opts.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*part
	for _, w := range waits {
		if v, ok := r.linkValues[w.id]; ok {
			out = append(out, r.stampLink(w, v.value))
			continue
		}
		if len(r.linkWaits) >= maxLinkWaits {
			out = append(out, r.stampLink(r.linkWaits[0], ""))
			r.linkWaits = r.linkWaits[1:]
		}
		w.at = now
		r.linkWaits = append(r.linkWaits, w)
	}
	return out
}

// releaseLinks routes the records that waited linkHold for their span, or, with all, every
// waiting record, unstamped, and forgets span values older than linkTTL. Take neither lock.
func (r *Relay) releaseLinks(all bool) {
	now := r.opts.Now()
	r.mu.Lock()
	var out []*part
	n := 0
	for _, w := range r.linkWaits {
		if !all && now.Sub(w.at) < linkHold {
			break
		}
		out = append(out, r.stampLink(w, ""))
		n++
	}
	r.linkWaits = r.linkWaits[n:]
	maps.DeleteFunc(r.linkValues, func(_ linkID, v linkValue) bool { return now.Sub(v.at) >= linkTTL })
	r.mu.Unlock()
	for _, p := range out {
		r.route(p)
	}
}

// stampLink sets the link's value on the waiting record, or counts it as leaving without one.
func (r *Relay) stampLink(w linkWait, value string) *part {
	if value == "" {
		r.stats.add("unlinked_records", 1)
		return w.p
	}
	to := r.rules.links[w.id.rule].To
	for _, rl := range w.p.msg.(*logspb.LogsData).GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				lr.Attributes = append(lr.Attributes, &commonpb.KeyValue{Key: to, Value: strValue(value)})
			}
		}
	}
	r.stats.add("linked_records", 1)
	return w.p
}

// evictLinkValueLocked forgets the oldest span value. r.mu is held.
func (r *Relay) evictLinkValueLocked() {
	var oldest linkID
	var at time.Time
	for id, v := range r.linkValues {
		if at.IsZero() || v.at.Before(at) {
			oldest, at = id, v.at
		}
	}
	delete(r.linkValues, oldest)
}
