package pi

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Each test replays real Pi events (testdata/replay, recorded on Pi 1.6.1 and on the floor,
// 1.5.0) through the rendered extension under Node and asserts on the parsed wire and the
// `terma hook` calls: one decision of the design each.

const canary = "THINKING-TEXT-NOT-FOR-EXPORT"

func both(t *testing.T, f func(t *testing.T, version string)) {
	for _, v := range []string{"1.6.1", "1.5.0"} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			f(t, v)
		})
	}
}

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func intAttr(t *testing.T, a otlpAttrs, k string) int64 {
	t.Helper()
	n, ok := a.int(k)
	if !ok {
		t.Errorf("%s missing (attrs %v)", k, keys(a))
	}
	return n
}

func keys(a otlpAttrs) []string {
	var out []string
	for k := range a {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func spanByID(spans []otlpSpan, id string) (otlpSpan, bool) {
	for _, s := range spans {
		if s.Attrs.str("gen_ai.tool.call.id") == id {
			return s, true
		}
	}
	return otlpSpan{}, false
}

func callByID(r *replayed, id string) (otlpSpan, bool) {
	for _, s := range r.chats() {
		if s.Attrs.str("pi.call.id") == id {
			return s, true
		}
	}
	return otlpSpan{}, false
}

// nextCall is the messageEntryId of the first turn_end after line i.
func nextCall(lines []line, i int) string {
	for _, l := range lines[i:] {
		if l["e"] == "turn_end" {
			return gstr(l, "event", "messageEntryId")
		}
	}
	return ""
}

// wireItems are every span and record sent, each serialised on its own: what a redelivery
// or a second run must reproduce byte for byte, whichever batches carried them.
func wireItems(t *testing.T, r *replayed) []string {
	t.Helper()
	var out []string
	for _, p := range r.Posts {
		if p.Status != 200 {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(p.Body), &body); err != nil {
			t.Fatal(err)
		}
		for _, rs := range toAnyList(body["resourceSpans"]) {
			for _, ss := range toAnyList(get(rs, "scopeSpans")) {
				for _, sp := range toAnyList(get(ss, "spans")) {
					data, _ := json.Marshal(sp)
					out = append(out, string(data))
				}
			}
		}
		for _, rl := range toAnyList(body["resourceLogs"]) {
			for _, sl := range toAnyList(get(rl, "scopeLogs")) {
				for _, lr := range toAnyList(get(sl, "logRecords")) {
					data, _ := json.Marshal(lr)
					out = append(out, string(data))
				}
			}
		}
	}
	return out
}

func toAnyList(v any) []any {
	l, _ := v.([]any)
	return l
}

// 01-print-baseline: one prompt, seven tools (a write, an edit, a failing command, a read,
// a grep, a find, an ls), eight calls.
func TestReplayPrintBaseline(t *testing.T) {
	t.Parallel()
	both(t, func(t *testing.T, v string) {
		path := recording(v, "01-print-baseline.1")
		lines := loadLines(t, path)
		r := replay(t, path, replayOptions{})
		sid := sessionIDs(lines)[0]
		cwd := gstr(events(lines, "session_start")[0], "ctx", "cwd")

		if len(r.Spans) == 0 || len(r.Records) == 0 {
			t.Fatalf("spans %d, records %d", len(r.Spans), len(r.Records))
		}
		check := func(what string, scope otlpScope, res, a otlpAttrs) {
			if scope != (otlpScope{Name: "terma-pi", Version: "2"}) {
				t.Errorf("%s scope = %+v", what, scope)
			}
			if res.str("service.name") != "pi" || res.str("telemetry.sdk.name") != "terma-pi" {
				t.Errorf("%s resource = %v", what, res)
			}
			eq(t, what+" session.id", a.str("session.id"), sid)
			eq(t, what+" pi.version", a.str("pi.version"), v)
			eq(t, what+" pi.mode", a.str("pi.mode"), "print")
			for _, k := range []string{"gen_ai.usage.total_tokens", "prompt_length", "pi.parent_session.id", "pi.session.forked_from.id"} {
				if a.has(k) {
					t.Errorf("%s carries %s", what, k)
				}
			}
		}
		for _, s := range r.Spans {
			check("span "+s.Name, s.Scope, s.Resource, s.Attrs)
		}
		for _, rec := range r.Records {
			check("record", rec.Scope, rec.Resource, rec.Attrs)
		}

		// One user entry: the prompt, and the turn of every call.
		var user map[string]any
		for _, e := range entries(lines) {
			if gstr(e.Entry, "type") == "message" && gstr(e.Entry, "message", "role") == "user" {
				user = e.Entry
			}
		}
		userID := gstr(user, "id")

		turnEnds := events(lines, "turn_end")
		calls := r.calls()
		if len(calls) != len(turnEnds) {
			t.Fatalf("chat spans = %d, turn_end = %d", len(calls), len(turnEnds))
		}
		for i, te := range turnEnds {
			c := calls[i]
			eq(t, "call id", c.Attrs.str("pi.call.id"), gstr(te, "event", "messageEntryId"))
			eq(t, "call turn", c.Attrs.str("pi.turn.id"), userID)
			start := ms(gnum(te, "event", "message", "timestamp"))
			eq(t, "call start", c.Start, start)
			eq(t, "call end", c.End, start+ms(gnum(te, "event", "message", "durationMs")))
			eq(t, "call kind", c.Kind, 3)
		}

		tools := r.tools()
		if len(tools) != 7 {
			t.Fatalf("tool spans = %d, want 7", len(tools))
		}
		for i, l := range lines {
			if l["e"] != "tool_execution_end" {
				continue
			}
			id := gstr(l, "event", "toolCallId")
			callID := nextCall(lines, i)
			s, ok := spanByID(tools, sid+":"+callID+":"+id)
			if !ok {
				t.Errorf("no tool span %s:%s:%s", sid, callID, id)
				continue
			}
			eq(t, "tool pi.call.id", s.Attrs.str("pi.call.id"), callID)
			eq(t, "tool pi.turn.id", s.Attrs.str("pi.turn.id"), userID)
			end := ms(gnum(l, "t"))
			eq(t, "tool end", s.End, end)
			eq(t, "tool start", s.Start, end-ms(gnum(l, "event", "durationMs")))
			eq(t, "tool kind", s.Kind, 1)
		}
		bash := r.tool("bash")[0]
		eq(t, "bash outcome", bash.Attrs.str("pi.tool.outcome"), "error")
		eq(t, "bash exit code", intAttr(t, bash.Attrs, "pi.tool.exit_code"), 3)
		eq(t, "bash status", bash.Status, 2)
		for name, file := range map[string]string{"write": "new.txt", "edit": "notes.txt", "read": "notes.txt"} {
			s := r.tool(name)[0]
			eq(t, name+" outcome", s.Attrs.str("pi.tool.outcome"), "ok")
			if got := s.Attrs.strs("pi.tool.files"); !slices.Equal(got, []string{cwd + "/" + file}) {
				t.Errorf("%s files = %v", name, got)
			}
		}
		for _, name := range []string{"grep", "find", "ls", "bash"} {
			if r.tool(name)[0].Attrs.has("pi.tool.files") {
				t.Errorf("%s names files", name)
			}
		}

		prompts := r.records("pi.user_prompt")
		if len(prompts) != 1 {
			t.Fatalf("prompts = %d", len(prompts))
		}
		p := prompts[0]
		text := textOfContent(get(user, "message", "content"))
		eq(t, "prompt entry", p.Attrs.str("pi.entry.id"), userID)
		eq(t, "prompt turn", p.Attrs.str("pi.turn.id"), userID)
		eq(t, "prompt length", intAttr(t, p.Attrs, "pi.prompt.length"), jsLen(text))
		eq(t, "prompt body", p.Body, text)
		eq(t, "prompt time", p.Time, isoNanos(t, gstr(user, "timestamp")))

		if strings.Contains(r.allBodies(), canary) {
			t.Errorf("the thinking text was exported")
		}

		for e, n := range map[string]int{"pi-session-start": 1, "pi-prompt": 1, "pi-session-end": 1, "pi-tool-reach": 7, "pi-file-edit": 2} {
			if got := len(r.hooks(e)); got != n {
				t.Errorf("%s calls = %d, want %d", e, got, n)
			}
		}
		want := map[string]map[string]any{
			"read":  {"paths": []string{cwd + "/notes.txt"}},
			"write": {"paths": []string{cwd + "/new.txt"}},
			"edit":  {"paths": []string{cwd + "/notes.txt"}},
			"grep":  {"paths": []string{cwd}, "globs": []string{"*.txt"}},
			"find":  {"paths": []string{cwd}, "globs": []string{"*.py"}},
			"ls":    {"lists": []string{cwd}},
			"bash":  {"command": "echo before-fail; exit 3"},
		}
		for _, h := range r.hooks("pi-tool-reach") {
			tool := gstr(h, "tool")
			w, ok := want[tool]
			if !ok {
				t.Errorf("reach for %s", tool)
				continue
			}
			for k, wv := range w {
				switch wv := wv.(type) {
				case []string:
					if got := toStrings(h[k]); !slices.Equal(got, wv) {
						t.Errorf("%s reach %s = %v, want %v", tool, k, got, wv)
					}
				case string:
					eq(t, tool+" reach "+k, gstr(h, k), wv)
				}
			}
			eq(t, tool+" reach session", gstr(h, "session_id"), sid)
		}
		for _, h := range r.hooks("pi-file-edit") {
			tool := gstr(h, "tool")
			s := r.tool(tool)[0]
			eq(t, tool+" edit tool_call_id", gstr(h, "tool_call_id"), s.Attrs.str("gen_ai.tool.call.id"))
			if got := toStrings(h["files"]); !slices.Equal(got, s.Attrs.strs("pi.tool.files")) || !filepath.IsAbs(got[0]) {
				t.Errorf("%s edit files = %v", tool, got)
			}
		}

		// The same events give the same bytes: derived ids, recorded times.
		again := replay(t, path, replayOptions{})
		if a, b := wireItems(t, r), wireItems(t, again); !slices.Equal(a, b) {
			t.Errorf("a second replay sent different items:\n%v\n%v", a, b)
		}
	})
}

// 03-resume: a session resumed in a second process is one session; the second process
// sends nothing of what the first did.
func TestReplayResume(t *testing.T) {
	t.Parallel()
	path1, path2 := recording("1.6.1", "03-resume.1"), recording("1.6.1", "03-resume.2")
	r1 := replay(t, path1, replayOptions{})
	r2 := replay(t, path2, replayOptions{})
	lines2 := loadLines(t, path2)
	if len(r1.Spans) == 0 || len(r2.Spans) == 0 {
		t.Fatalf("spans %d, %d", len(r1.Spans), len(r2.Spans))
	}
	sessions := map[string]bool{}
	for _, r := range []*replayed{r1, r2} {
		for _, a := range r.everyAttrs() {
			sessions[a.str("session.id")] = true
		}
	}
	if len(sessions) != 1 {
		t.Errorf("sessions = %v", sessions)
	}
	sid := sessionIDs(lines2)[0]
	held := entriesBefore(lines2, sid, indexOf(lines2, func(l line) bool { return l["e"] == "session_start" }))
	for _, a := range r2.everyAttrs() {
		if a.str("event.name") == "pi.session.title" {
			continue
		}
		for _, k := range []string{"pi.call.id", "pi.entry.id"} {
			if held[a.str(k)] {
				t.Errorf("the second process sent %s %s of the first's", k, a.str(k))
			}
		}
	}
	first := r1.ids()
	for _, id := range r2.ids() {
		if slices.Contains(first, id) && !strings.Contains(id, "pi.entry.id") {
			t.Errorf("id %s sent by both processes", id)
		}
	}
}

// 04-fork-flag: a fork is a new session naming its source; nothing it copied is sent.
func TestReplayForkFlag(t *testing.T) {
	t.Parallel()
	path := recording("1.6.1", "04-fork-flag.1")
	lines := loadLines(t, path)
	r := replay(t, path, replayOptions{})
	sess := events(lines, "_session")[0]
	sid, source := gstr(sess, "sessionId"), gstr(sess, "parentHeaderId")
	if source == "" || source == sid {
		t.Fatalf("recording: source %q, session %q", source, sid)
	}
	if len(r.Spans) == 0 || len(r.Records) == 0 {
		t.Fatalf("spans %d, records %d", len(r.Spans), len(r.Records))
	}
	for _, a := range r.everyAttrs() {
		eq(t, "session", a.str("session.id"), sid)
		eq(t, "forked_from", a.str("pi.session.forked_from.id"), source)
	}
	copied := entriesBefore(lines, sid, indexOf(lines, func(l line) bool { return l["e"] == "session_start" }))
	for _, a := range r.everyAttrs() {
		for _, k := range []string{"pi.call.id", "pi.entry.id"} {
			if copied[a.str(k)] {
				t.Errorf("the fork sent the copied entry %s", a.str(k))
			}
		}
	}
	// Its source ran in another directory: that directory is reach-checked right after the
	// start, and the name the fork copied is not sent.
	srcCwd := gstr(sess, "parentHeaderCwd")
	if srcCwd == "" || srcCwd == gstr(events(lines, "session_start")[0], "ctx", "cwd") {
		t.Fatalf("recording: source cwd %q", srcCwd)
	}
	if len(r.Hooks) < 2 || r.Hooks[0].Event != "pi-session-start" || r.Hooks[1].Event != "pi-tool-reach" ||
		!slices.Equal(toStrings(r.Hooks[1].Payload["paths"]), []string{srcCwd}) {
		t.Errorf("hooks %+v", r.Hooks)
	}
	// The recording's source has no name: give the fork a copied one.
	named := insertAt(lines, indexOf(lines, func(l line) bool { return l["e"] == "session_start" }), line{"e": "_entries", "sessionId": sid,
		"entries": []any{map[string]any{"type": "session_info", "id": "i0000001", "parentId": nil, "timestamp": "2026-11-27T15:34:30.000Z", "name": "Source's name"}}})
	if n := len(replay(t, writeLines(t, named), replayOptions{}).records("pi.session.title")); n != 0 {
		t.Errorf("the fork sent %d inherited titles", n)
	}
}

// A clone or fork in its source's directory reaches nothing for its source.
func TestReplaySameDirectoryForkReachesNothing(t *testing.T) {
	t.Parallel()
	for _, c := range []string{"1.6.1/04-new-fork-clone-tree", "1.5.0/04-new-fork-clone-tree", "1.6.1/22-clone-after-custom"} {
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			v, name, _ := strings.Cut(c, "/")
			path := recording(v, name+".1")
			sources := map[string]bool{}
			for _, l := range events(loadLines(t, path), "_session") {
				if cwd := gstr(l, "parentHeaderCwd"); cwd != "" {
					sources[cwd] = true
				}
			}
			if len(sources) == 0 {
				t.Fatal("recording: no fork names its source's directory")
			}
			for _, h := range replay(t, path, replayOptions{}).hooks("pi-tool-reach") {
				if p := toStrings(h["paths"]); len(p) == 1 && sources[p[0]] {
					t.Errorf("a reach for the source's directory %s", p[0])
				}
			}
		})
	}
}

// 04-new-fork-clone-tree: /clone, /fork (with /tree), /new and /resume in one process.
func TestReplayNewForkCloneTree(t *testing.T) {
	t.Parallel()
	both(t, func(t *testing.T, v string) {
		path := recording(v, "04-new-fork-clone-tree.1")
		lines := loadLines(t, path)
		r := replay(t, path, replayOptions{})

		var starts, ends []string
		for _, l := range events(lines, "session_start") {
			if gstr(l, "event", "reason") != "reload" {
				starts = append(starts, gstr(l, "ctx", "sessionId"))
			}
		}
		for _, l := range events(lines, "session_shutdown") {
			if gstr(l, "event", "reason") != "reload" {
				ends = append(ends, gstr(l, "ctx", "sessionId"))
			}
		}
		var gotStarts, gotEnds []string
		for _, h := range r.hooks("pi-session-start") {
			gotStarts = append(gotStarts, gstr(h, "session_id"))
		}
		for _, h := range r.hooks("pi-session-end") {
			gotEnds = append(gotEnds, gstr(h, "session_id"))
		}
		if !slices.Equal(gotStarts, starts) {
			t.Errorf("session starts = %v, want %v", gotStarts, starts)
		}
		if !slices.Equal(gotEnds, ends) {
			t.Errorf("session ends = %v, want %v", gotEnds, ends)
		}

		forked := map[string]string{}
		for _, l := range events(lines, "_session") {
			if src := gstr(l, "parentHeaderId"); src != "" {
				forked[gstr(l, "sessionId")] = src
			}
		}
		if len(forked) < 2 {
			t.Fatalf("recording: forks %v", forked)
		}
		sent := map[string]bool{}
		for _, a := range r.everyAttrs() {
			sid := a.str("session.id")
			sent[sid] = true
			eq(t, sid+" forked_from", a.str("pi.session.forked_from.id"), forked[sid])
		}
		for _, sid := range sessionIDs(lines) {
			if !sent[sid] {
				t.Errorf("nothing sent for session %s", sid)
			}
		}
		seen := map[string]bool{}
		for _, id := range r.ids() {
			if seen[id] && !strings.Contains(id, "pi.entry.id") {
				t.Errorf("id sent twice: %s", id)
			}
			seen[id] = true
		}
		// Every name a session holds is its title: the rename in the source, and the name a
		// clone or fork starts with, under its own session.
		titles := map[string]otlpRecord{}
		for _, rec := range r.records("pi.session.title") {
			k := rec.Attrs.str("session.id") + "/" + rec.Attrs.str("pi.entry.id")
			if _, dup := titles[k]; dup {
				t.Errorf("title %s sent twice", k)
			}
			titles[k] = rec
		}
		want := 0
		for _, e := range entries(lines) {
			if gstr(e.Entry, "type") != "session_info" {
				continue
			}
			want++
			rec, ok := titles[e.Session+"/"+gstr(e.Entry, "id")]
			if !ok {
				t.Errorf("no title %s for session %s", gstr(e.Entry, "id"), e.Session)
				continue
			}
			eq(t, "title body", rec.Body, gstr(e.Entry, "name"))
			eq(t, "title time", rec.Time, isoNanos(t, gstr(e.Entry, "timestamp")))
		}
		if want < 2 || len(titles) != want {
			t.Errorf("titles = %d, want %d", len(titles), want)
		}
	})
}

// checkCompactions: one compaction span per compaction entry, with the entry's id, usage
// and cost, the reason kept at session_before_compact and the model at session_compact.
func checkCompactions(t *testing.T, lines []line, r *replayed) []otlpSpan {
	t.Helper()
	var out []otlpSpan
	var comps []map[string]any
	for _, e := range entries(lines) {
		if gstr(e.Entry, "type") == "compaction" {
			comps = append(comps, e.Entry)
		}
	}
	for _, s := range r.chats() {
		if s.Attrs.str("pi.call.kind") == "compaction" {
			out = append(out, s)
		}
	}
	if len(comps) == 0 || len(out) != len(comps) {
		t.Fatalf("compaction spans = %d, entries = %d", len(out), len(comps))
	}
	for _, c := range comps {
		id := gstr(c, "id")
		s, ok := callByID(r, id)
		if !ok {
			t.Errorf("no compaction span %s", id)
			continue
		}
		i := indexOf(lines, func(l line) bool {
			if l["e"] != "_entries" {
				return false
			}
			for _, e := range toAnyList(l["entries"]) {
				if gstr(e, "id") == id {
					return true
				}
			}
			return false
		})
		var before, after line
		for j := i; j >= 0; j-- {
			if lines[j]["e"] == "session_before_compact" {
				before = lines[j]
				break
			}
		}
		for _, l := range lines[i:] {
			if l["e"] == "session_compact" {
				after = l
				break
			}
		}
		eq(t, "compaction reason", s.Attrs.str("pi.compaction.reason"), gstr(before, "event", "reason"))
		eq(t, "compaction model", s.Attrs.str("gen_ai.request.model"), gstr(after, "ctx", "modelId"))
		eq(t, "compaction provider", s.Attrs.str("gen_ai.provider.name"), gstr(after, "ctx", "modelProvider"))
		eq(t, "compaction tokens before", intAttr(t, s.Attrs, "pi.compaction.tokens_before"), int64(gnum(c, "tokensBefore")))
		eq(t, "compaction input", intAttr(t, s.Attrs, "gen_ai.usage.input_tokens"), int64(gnum(c, "usage", "input")))
		eq(t, "compaction output", intAttr(t, s.Attrs, "gen_ai.usage.output_tokens"), int64(gnum(c, "usage", "output")))
		cost, ok := s.Attrs.double("gen_ai.usage.total_cost")
		if !ok || cost != gnum(c, "usage", "cost", "total") {
			t.Errorf("compaction cost = %v (%v), want %v", cost, ok, gnum(c, "usage", "cost", "total"))
		}
		if s.Attrs.has("pi.turn.id") {
			t.Errorf("a compaction carries a turn")
		}
		eq(t, "compaction end", s.End, isoNanos(t, gstr(c, "timestamp")))
		if s.Start > s.End || s.Start != ms(gnum(before, "t")) {
			t.Errorf("compaction start %d, end %d, before_compact at %d", s.Start, s.End, ms(gnum(before, "t")))
		}
		eq(t, "compaction summary", s.Attrs.str("pi.compaction.summary"), gstr(c, "summary"))
	}
	return out
}

func TestReplayCompactionThreshold(t *testing.T) {
	t.Parallel()
	path := recording("1.6.1", "05-compaction-threshold.1")
	lines := loadLines(t, path)
	spans := checkCompactions(t, lines, replay(t, path, replayOptions{}))
	eq(t, "reason", spans[0].Attrs.str("pi.compaction.reason"), "threshold")
}

func TestReplayTwoCompactions(t *testing.T) {
	t.Parallel()
	both(t, func(t *testing.T, v string) {
		path := recording(v, "23-two-compactions.1")
		lines := loadLines(t, path)
		spans := checkCompactions(t, lines, replay(t, path, replayOptions{}))
		var models []string
		for _, s := range spans {
			eq(t, "reason", s.Attrs.str("pi.compaction.reason"), "manual")
			models = append(models, s.Attrs.str("gen_ai.request.model"))
		}
		if !slices.Equal(models, []string{"m", "m2"}) {
			t.Errorf("models = %v", models)
		}
	})
}

// Two compactions with the same summary, and an event that names the first entry both
// times (Pi picks the event's entry by its summary text): still two calls, each its own.
func TestReplayCompactionsWithOneSummary(t *testing.T) {
	t.Parallel()
	lines := loadLines(t, recording("1.6.1", "23-two-compactions.1"))
	var first any
	for _, l := range lines {
		if l["e"] == "_entries" {
			for _, e := range toAnyList(l["entries"]) {
				if gstr(e, "type") == "compaction" {
					e.(map[string]any)["summary"] = "The same summary."
					if first == nil {
						first = e
					}
				}
			}
		}
	}
	for _, l := range events(lines, "session_compact") {
		l["event"].(map[string]any)["compactionEntry"] = first
	}
	r := replay(t, writeLines(t, lines), replayOptions{})
	spans := checkCompactions(t, lines, r)
	if spans[0].Attrs.str("pi.call.id") == spans[1].Attrs.str("pi.call.id") || spans[0].SpanID == spans[1].SpanID {
		t.Errorf("the two compactions share an id")
	}
}

// draftCompaction is a compaction entry an extension returned as a draft: Pi stores it with
// no session_before_compact and no session_compact.
func draftCompaction(id, parent, timestamp string) line {
	return line{"e": "_entries", "entries": []any{map[string]any{
		"type": "compaction", "id": id, "parentId": parent, "timestamp": timestamp,
		"summary": "A draft summary.", "tokensBefore": 42.0, "fromHook": true,
		"usage": map[string]any{"input": 7.0, "output": 3.0, "cacheRead": 0.0, "cacheWrite": 0.0, "reasoning": 0.0, "totalTokens": 10.0,
			"cost": map[string]any{"total": 0.001, "totalFromProvider": false}},
	}}}
}

func insertAt(lines []line, i int, add ...line) []line {
	return append(lines[:i:i], append(add, lines[i:]...)...)
}

func lastIndex(lines []line, name string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i]["e"] == name {
			return i
		}
	}
	return -1
}

// A draft compaction is sent at the read that finds it, the next turn_end here, with no
// reason and no model; and session_compact_failed clears a kept reason, so a later draft
// does not inherit it.
func TestReplayDraftCompaction(t *testing.T) {
	t.Parallel()
	t.Run("at turn_end", func(t *testing.T) {
		t.Parallel()
		lines := loadLines(t, recording("1.6.1", "01-print-baseline.1"))
		i := lastIndex(lines, "turn_end")
		sid := gstr(lines[i], "ctx", "sessionId")
		d := draftCompaction("d0d0d0d0", gstr(lines[i], "event", "messageEntryId"), "2026-11-27T15:34:36.081Z")
		d["sessionId"] = sid
		lines = insertAt(lines, i, d)
		r := replay(t, writeLines(t, lines), replayOptions{})
		s, ok := callByID(r, "d0d0d0d0")
		if !ok {
			t.Fatalf("no span for the draft compaction")
		}
		eq(t, "kind", s.Attrs.str("pi.call.kind"), "compaction")
		for _, k := range []string{"pi.compaction.reason", "gen_ai.request.model", "gen_ai.provider.name", "pi.auth.oauth", "pi.turn.id"} {
			if s.Attrs.has(k) {
				t.Errorf("a draft compaction carries %s", k)
			}
		}
		eq(t, "input", intAttr(t, s.Attrs, "gen_ai.usage.input_tokens"), 7)
		eq(t, "start", s.Start, s.End)
	})
	t.Run("after a failed compaction", func(t *testing.T) {
		t.Parallel()
		lines := loadLines(t, recording("1.6.1", "23-two-compactions.1"))
		c := lastIndex(lines, "session_compact")
		ctx := lines[c]["ctx"]
		at := gnum(lines[c], "t")
		lines = insertAt(lines, c+1,
			line{"e": "session_before_compact", "t": at + 10, "event": map[string]any{"reason": "threshold", "willRetry": false}, "ctx": ctx},
			line{"e": "session_compact_failed", "t": at + 20, "event": map[string]any{"reason": "threshold"}, "ctx": ctx},
		)
		i := lastIndex(lines, "turn_end")
		d := draftCompaction("d1d1d1d1", gstr(lines[i], "event", "messageEntryId"), "2026-11-27T15:37:52.900Z")
		d["sessionId"] = gstr(lines[i], "ctx", "sessionId")
		lines = insertAt(lines, i, d)
		r := replay(t, writeLines(t, lines), replayOptions{})
		s, ok := callByID(r, "d1d1d1d1")
		if !ok {
			t.Fatalf("no span for the draft compaction")
		}
		if s.Attrs.has("pi.compaction.reason") {
			t.Errorf("the draft inherited a failed compaction's reason %q", s.Attrs.str("pi.compaction.reason"))
		}
		eq(t, "start", s.Start, s.End)
	})
}

// A failed or aborted call: its own span, with its status, no zero counts, no zero cost.
func TestReplayFailedCalls(t *testing.T) {
	t.Parallel()
	t.Run("07-error-400", func(t *testing.T) {
		t.Parallel()
		r := replay(t, recording("1.6.1", "07-error-400.1"), replayOptions{})
		calls := r.calls()
		if len(calls) != 1 {
			t.Fatalf("calls = %d", len(calls))
		}
		c := calls[0]
		eq(t, "status", c.Status, 2)
		eq(t, "finish", c.Attrs.str("gen_ai.response.finish_reasons"), "error")
		if c.Attrs.str("pi.error.message") == "" {
			t.Errorf("no error message")
		}
		for k := range c.Attrs {
			if strings.HasPrefix(k, "gen_ai.usage.") {
				t.Errorf("a call that never ran carries %s", k)
			}
		}
	})
	t.Run("07-retry-500", func(t *testing.T) {
		t.Parallel()
		r := replay(t, recording("1.6.1", "07-retry-500.1"), replayOptions{})
		calls := r.calls()
		failed := 0
		turns := map[string]bool{}
		for _, c := range calls {
			if c.Status == 2 {
				failed++
			}
			turns[c.Attrs.str("pi.turn.id")] = true
		}
		if len(calls) != 3 || failed != 2 || len(turns) != 1 || turns[""] {
			t.Errorf("calls %d, failed %d, turns %v", len(calls), failed, turns)
		}
	})
	t.Run("07-stream-cut", func(t *testing.T) {
		t.Parallel()
		r := replay(t, recording("1.6.1", "07-stream-cut.1"), replayOptions{})
		failed := 0
		for _, c := range r.calls() {
			if c.Status == 2 {
				failed++
				eq(t, "finish", c.Attrs.str("gen_ai.response.finish_reasons"), "error")
			}
		}
		if failed == 0 || len(r.calls()) < 2 {
			t.Errorf("calls %d, failed %d", len(r.calls()), failed)
		}
	})
	t.Run("07-aborts", func(t *testing.T) {
		t.Parallel()
		path := recording("1.6.1", "07-aborts.1")
		lines := loadLines(t, path)
		r := replay(t, path, replayOptions{})
		checked := 0
		for _, te := range events(lines, "turn_end") {
			if gstr(te, "event", "message", "stopReason") != "aborted" {
				continue
			}
			c, ok := callByID(r, gstr(te, "event", "messageEntryId"))
			if !ok {
				t.Errorf("no span for the aborted call")
				continue
			}
			eq(t, "aborted status", c.Status, 0)
			eq(t, "aborted finish", c.Attrs.str("gen_ai.response.finish_reasons"), "aborted")
			if text := textOfContent(get(te, "event", "message", "content")); text != "" {
				checked++
				eq(t, "aborted reply", c.Attrs.str("gen_ai.completion"), text)
				eq(t, "aborted reply length", intAttr(t, c.Attrs, "pi.completion.length"), jsLen(text))
			}
		}
		if checked == 0 {
			t.Errorf("no aborted call with its cut text")
		}
		bash := r.tool("bash")
		if len(bash) != 1 || bash[0].Attrs.str("pi.tool.outcome") != "error" {
			t.Errorf("aborted bash = %+v", bash)
		}
	})
}

// A call that never ran (blocked, unknown, invalid) is not_run, starts when it was asked
// for, and carries the reason; a failed edit is an error and hands terma no edit.
func TestReplayToolOutcomes(t *testing.T) {
	t.Parallel()
	for _, c := range []string{"07-blocked-call", "07-unknown-tool", "07-invalid-args"} {
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			path := recording("1.6.1", c+".1")
			lines := loadLines(t, path)
			r := replay(t, path, replayOptions{})
			sid := sessionIDs(lines)[0]
			checked := 0
			for i, l := range lines {
				if l["e"] != "tool_execution_end" || get(l, "event", "durationMs") != nil {
					continue
				}
				id := gstr(l, "event", "toolCallId")
				s, ok := spanByID(r.tools(), sid+":"+nextCall(lines, i)+":"+id)
				if !ok {
					t.Errorf("no span for %s", id)
					continue
				}
				checked++
				eq(t, "outcome", s.Attrs.str("pi.tool.outcome"), "not_run")
				eq(t, "status", s.Status, 2)
				startLine := lines[indexOf(lines, func(x line) bool {
					return x["e"] == "tool_execution_start" && gstr(x, "event", "toolCallId") == id
				})]
				eq(t, "start", s.Start, ms(gnum(startLine, "t")))
				eq(t, "end", s.End, ms(gnum(l, "t")))
				reason := textOfContent(get(l, "event", "result", "content"))
				if reason == "" || s.Attrs.str("gen_ai.tool.call.result") != reason {
					t.Errorf("result = %q, want %q", s.Attrs.str("gen_ai.tool.call.result"), reason)
				}
			}
			if checked == 0 {
				t.Errorf("no call that did not run")
			}
		})
	}
	t.Run("07-failed-edit", func(t *testing.T) {
		t.Parallel()
		r := replay(t, recording("1.6.1", "07-failed-edit.1"), replayOptions{})
		edits := r.tool("edit")
		if len(edits) == 0 {
			t.Fatalf("no edit span")
		}
		for _, s := range edits {
			eq(t, "outcome", s.Attrs.str("pi.tool.outcome"), "error")
		}
		if n := len(r.hooks("pi-file-edit")); n != 0 {
			t.Errorf("pi-file-edit calls = %d for a failed edit", n)
		}
	})
	t.Run("07-two-calls-one-reply", func(t *testing.T) {
		t.Parallel()
		r := replay(t, recording("1.6.1", "07-two-calls-one-reply.1"), replayOptions{})
		tools := r.tools()
		if len(tools) != 2 {
			t.Fatalf("tool spans = %d", len(tools))
		}
		eq(t, "call", tools[0].Attrs.str("pi.call.id"), tools[1].Attrs.str("pi.call.id"))
		if tools[0].Attrs.str("gen_ai.tool.call.id") == tools[1].Attrs.str("gen_ai.tool.call.id") {
			t.Errorf("two calls share an id")
		}
	})
}

// 07-paths: a file URL, ~ and @ reach the hook and the span as the file Pi opens.
func TestReplayPaths(t *testing.T) {
	t.Parallel()
	path := recording("1.6.1", "07-paths.1")
	lines := loadLines(t, path)
	r := replay(t, path, replayOptions{})
	cwd := gstr(events(lines, "session_start")[0], "ctx", "cwd")
	want := map[string]string{
		"file:///home/user/work/other-1.6.1/notes.txt": "/home/user/work/other-1.6.1/notes.txt",
		"~/tilde/note.txt":                      "/home/user/tilde/note.txt",
		"@notes.txt":                            cwd + "/notes.txt",
		"@at.txt":                               cwd + "/at.txt",
		"/home/user/work/delta-1.6.1/abs.txt":   "/home/user/work/delta-1.6.1/abs.txt",
		"/home/user/work/other-1.6.1/notes.txt": "/home/user/work/other-1.6.1/notes.txt",
	}
	sid := sessionIDs(lines)[0]
	reaches := r.hooks("pi-tool-reach")
	edits := r.hooks("pi-file-edit")
	for i, l := range lines {
		if l["e"] != "tool_execution_end" {
			continue
		}
		start := lines[indexOf(lines, func(x line) bool {
			return x["e"] == "tool_execution_start" && gstr(x, "event", "toolCallId") == gstr(l, "event", "toolCallId")
		})]
		arg := gstr(start, "event", "args", "path")
		w, ok := want[arg]
		if !ok {
			t.Fatalf("recording: path %q", arg)
		}
		s, ok := spanByID(r.tools(), sid+":"+nextCall(lines, i)+":"+gstr(l, "event", "toolCallId"))
		if !ok {
			t.Errorf("no span for %q", arg)
			continue
		}
		if got := s.Attrs.strs("pi.tool.files"); !slices.Equal(got, []string{w}) {
			t.Errorf("%q files = %v, want %s", arg, got, w)
		}
		var found bool
		switch gstr(start, "event", "toolName") {
		case "read":
			for _, h := range reaches {
				found = found || slices.Equal(toStrings(h["paths"]), []string{w})
			}
		case "write":
			for _, h := range edits {
				found = found || slices.Equal(toStrings(h["files"]), []string{w})
			}
		}
		if !found {
			t.Errorf("no hook judged %q as %s", arg, w)
		}
	}
}

// The resolver table: Pi's own resolveToCwd on each input, for read (paths and files), edit,
// write, grep and find (paths) and ls (lists).
func TestReplayPathTable(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("testdata", "resolve_paths.json"))
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Cwd   string `json:"cwd"`
		Home  string `json:"home"`
		Cases []struct {
			Input    string `json:"input"`
			Resolved string `json:"resolved"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatal(err)
	}
	if table.Home != "/home/user" {
		t.Fatalf("table home %q", table.Home)
	}
	const sid = "01a21daf-0000-7000-8000-000000000001"
	ctx := map[string]any{"sessionId": sid, "leafId": nil, "mode": "print", "modelProvider": "fake", "modelId": "m", "cwd": table.Cwd, "oauth": false}
	at := 1795793600000.0
	ev := func(name string, event map[string]any) line {
		at++
		return line{"e": name, "t": at, "event": event, "ctx": ctx}
	}
	lines := []line{
		{"e": "_process", "pid": 1.0, "ppid": 1.0, "argv": []any{}, "version": "1.6.1", "inheritedParent": nil},
		{"e": "_session", "sessionId": sid, "sessionFile": "", "header": map[string]any{"type": "session", "id": sid, "cwd": table.Cwd}},
		ev("session_start", map[string]any{"reason": "startup"}),
	}
	type want struct {
		tool, key string
		value     []string
	}
	var wants []want
	var files [][]string
	for i, c := range table.Cases {
		id := fmt.Sprintf("call_%d", i)
		if c.Input != "" {
			lines = append(lines,
				ev("tool_execution_start", map[string]any{"toolCallId": id + "_read", "toolName": "read", "args": map[string]any{"path": c.Input}}),
				ev("tool_call", map[string]any{"toolCallId": id + "_read", "toolName": "read", "input": map[string]any{"path": c.Input}}),
				ev("tool_execution_end", map[string]any{"toolCallId": id + "_read", "toolName": "read", "result": map[string]any{"content": []any{}}, "isError": false, "durationMs": 1.0}),
			)
			wants = append(wants, want{"read", "paths", []string{c.Resolved}})
			files = append(files, []string{c.Resolved})
			for _, tool := range []string{"edit", "write"} {
				lines = append(lines, ev("tool_call", map[string]any{"toolCallId": id + "_" + tool, "toolName": tool, "input": map[string]any{"path": c.Input}}))
				wants = append(wants, want{tool, "paths", []string{c.Resolved}})
			}
		}
		lines = append(lines,
			ev("tool_call", map[string]any{"toolCallId": id + "_grep", "toolName": "grep", "input": map[string]any{"pattern": "x", "path": c.Input}}),
			ev("tool_call", map[string]any{"toolCallId": id + "_find", "toolName": "find", "input": map[string]any{"pattern": "*.go", "path": c.Input}}),
			ev("tool_call", map[string]any{"toolCallId": id + "_ls", "toolName": "ls", "input": map[string]any{"path": c.Input}}),
		)
		wants = append(wants, want{"grep", "paths", []string{c.Resolved}}, want{"find", "paths", []string{c.Resolved}}, want{"ls", "lists", []string{c.Resolved}})
	}
	lines = append(lines,
		ev("turn_end", map[string]any{"messageEntryId": "e0000001", "message": map[string]any{"role": "assistant", "provider": "fake", "model": "m",
			"stopReason": "toolUse", "timestamp": at, "durationMs": 1.0, "content": []any{}}}),
		ev("session_shutdown", map[string]any{"reason": "quit"}),
	)
	r := replay(t, writeLines(t, lines), replayOptions{})
	reaches := r.hooks("pi-tool-reach")
	if len(reaches) != len(wants) {
		t.Fatalf("reach calls = %d, want %d", len(reaches), len(wants))
	}
	for i, w := range wants {
		h := reaches[i]
		eq(t, "tool", gstr(h, "tool"), w.tool)
		if got := toStrings(h[w.key]); !slices.Equal(got, w.value) {
			t.Errorf("%s %s = %v, want %v", w.tool, w.key, got, w.value)
		}
	}
	reads := r.tool("read")
	if len(reads) != len(files) {
		t.Fatalf("read spans = %d, want %d", len(reads), len(files))
	}
	for i, s := range reads {
		if got := s.Attrs.strs("pi.tool.files"); !slices.Equal(got, files[i]) {
			t.Errorf("read files = %v, want %v", got, files[i])
		}
	}
}

// 08-user-bash: a person's `!` command is checked for what it reaches; `!!` is not; neither
// is exported.
func TestReplayUserBash(t *testing.T) {
	t.Parallel()
	path := recording("1.6.1", "08-user-bash.1")
	lines := loadLines(t, path)
	r := replay(t, path, replayOptions{})
	var want []string
	var hidden []string
	for _, l := range events(lines, "user_bash") {
		if get(l, "event", "excludeFromContext") == true {
			hidden = append(hidden, gstr(l, "event", "command"))
		} else {
			want = append(want, gstr(l, "event", "command"))
		}
	}
	var got []string
	for _, h := range r.hooks("pi-tool-reach") {
		eq(t, "tool", gstr(h, "tool"), "bash")
		got = append(got, gstr(h, "command"))
		if gstr(h, "dir") == "" {
			t.Errorf("no dir for %q", gstr(h, "command"))
		}
	}
	if !slices.Equal(got, want) || len(hidden) == 0 {
		t.Errorf("reach commands = %q, want %q (hidden %q)", got, want, hidden)
	}
	if len(r.tools()) != 0 {
		t.Errorf("tool spans = %d", len(r.tools()))
	}
	bodies := r.allBodies()
	for _, c := range append(want, hidden...) {
		if strings.Contains(bodies, c) {
			t.Errorf("command %q exported", c)
		}
	}
}

// 09-codemode: nested calls are qualified by the outer call's entry; the nested write's edit
// carries that id; the codemode span names no files.
func TestReplayCodemode(t *testing.T) {
	t.Parallel()
	both(t, func(t *testing.T, v string) {
		path := recording(v, "09-codemode.1")
		lines := loadLines(t, path)
		r := replay(t, path, replayOptions{})
		sid := sessionIDs(lines)[0]
		outer := indexOf(lines, func(l line) bool {
			return l["e"] == "tool_execution_start" && gstr(l, "event", "toolName") == "codemode"
		})
		outerID := gstr(lines[outer], "event", "toolCallId")
		entry := nextCall(lines, outer)
		nested := 0
		for _, l := range events(lines, "tool_execution_start") {
			if gstr(l, "event", "parentToolCallId") != outerID {
				continue
			}
			nested++
			id := gstr(l, "event", "toolCallId")
			if !strings.HasPrefix(id, outerID+"/") {
				t.Errorf("nested id %s", id)
			}
			qid := sid + ":" + entry + ":" + id
			if _, ok := spanByID(r.tools(), qid); !ok {
				t.Errorf("no span %s", qid)
			}
			if gstr(l, "event", "toolName") == "write" {
				var found bool
				for _, h := range r.hooks("pi-file-edit") {
					found = found || gstr(h, "tool_call_id") == qid
				}
				if !found {
					t.Errorf("no pi-file-edit with %s; edits %v", qid, r.hooks("pi-file-edit"))
				}
			}
		}
		if nested < 2 {
			t.Errorf("nested calls = %d", nested)
		}
		cm := r.tool("codemode")
		if len(cm) != 1 || cm[0].Attrs.has("pi.tool.files") {
			t.Errorf("codemode spans = %+v", cm)
		}
	})
}

func TestReplayMCPAndCustomTools(t *testing.T) {
	t.Parallel()
	t.Run("09-mcp", func(t *testing.T) {
		t.Parallel()
		path := recording("1.6.1", "09-mcp.1")
		lines := loadLines(t, path)
		r := replay(t, path, replayOptions{})
		var named, failed int
		for _, s := range r.tools() {
			if s.Attrs.str("pi.mcp.server") != "" && s.Attrs.str("pi.mcp.tool") != "" {
				named++
			}
			if s.Attrs.str("pi.tool.outcome") == "error" {
				failed++
			}
		}
		var wantFailed int
		for _, l := range events(lines, "tool_execution_end") {
			if get(l, "event", "isError") == true && get(l, "event", "durationMs") != nil {
				wantFailed++
			}
		}
		if named == 0 || failed == 0 || failed != wantFailed {
			t.Errorf("MCP-named spans %d, failed %d (want %d)", named, failed, wantFailed)
		}
	})
	t.Run("09-custom-tool", func(t *testing.T) {
		t.Parallel()
		path := recording("1.6.1", "09-custom-tool.1")
		lines := loadLines(t, path)
		r := replay(t, path, replayOptions{})
		if n := len(events(lines, "tool_execution_end")); len(r.tools()) != n || n == 0 {
			t.Errorf("tool spans = %d, want %d", len(r.tools()), n)
		}
		for _, s := range r.tools() {
			if s.Attrs.str("gen_ai.tool.call.id") == "" || s.Attrs.str("pi.tool.outcome") == "" || s.Attrs.str("pi.call.id") == "" {
				t.Errorf("tool span %v", keys(s.Attrs))
			}
		}
	})
}

// 10-subagent: a child pi inherits its parent's session through TERMA_PI_PARENT, accepted
// only from an ancestor process.
func TestReplaySubagent(t *testing.T) {
	t.Parallel()
	both(t, func(t *testing.T, v string) {
		parentPath := recording(v, "10-subagent.1")
		parentLines := loadLines(t, parentPath)
		parent := sessionIDs(parentLines)[0]
		pr := replay(t, parentPath, replayOptions{})
		if pr.EnvParent == nil || *pr.EnvParent != fmt.Sprintf("%d:%s", pr.PID, parent) {
			t.Errorf("TERMA_PI_PARENT left = %v, want %d:%s", pr.EnvParent, pr.PID, parent)
		}
		for _, n := range []string{"2", "3"} {
			path := recording(v, "10-subagent."+n)
			// This test is the driver's parent, so the ancestor check passes for real.
			r := replay(t, path, replayOptions{Parent: fmt.Sprintf("%d:%s", os.Getpid(), parent)})
			if len(r.Spans) == 0 || len(r.Records) == 0 {
				t.Fatalf("child %s: spans %d, records %d", n, len(r.Spans), len(r.Records))
			}
			for _, a := range r.everyAttrs() {
				eq(t, "child "+n+" parent", a.str("pi.parent_session.id"), parent)
				if a.has("pi.turn.id") {
					t.Errorf("child %s: a turn on %s", n, a.str("event.name"))
				}
			}
			if len(r.records("pi.session.title")) != 0 {
				t.Errorf("child %s sent a title", n)
			}
			for _, e := range []string{"pi-session-start", "pi-tool-reach", "pi-file-edit"} {
				for _, h := range r.hooks(e) {
					eq(t, "child "+n+" "+e+" parent", gstr(h, "parent_session_id"), parent)
				}
			}
			if len(r.hooks("pi-session-start")) == 0 {
				t.Errorf("child %s: no session start", n)
			}

			// The pid of a live process that is not an ancestor links nothing.
			other := exec.Command("sleep", "30")
			if err := other.Start(); err != nil {
				t.Fatal(err)
			}
			u := replay(t, path, replayOptions{Parent: fmt.Sprintf("%d:%s", other.Process.Pid, parent)})
			_ = other.Process.Kill()
			_ = other.Wait()
			for _, a := range u.everyAttrs() {
				if a.has("pi.parent_session.id") {
					t.Errorf("unrelated pid: linked to %s", a.str("pi.parent_session.id"))
				}
			}
			for _, h := range u.Hooks {
				if _, ok := h.Payload["parent_session_id"]; ok {
					t.Errorf("unrelated pid: %s names a parent", h.Event)
				}
			}
		}
	})
}

// 10-subagent-inprocess: a session nested inside the process fires no start; it is claimed
// at its first event, before its prompt is stored, so its prompt is sent; it has no parent.
func TestReplayNestedSession(t *testing.T) {
	t.Parallel()
	both(t, func(t *testing.T, v string) {
		path := recording(v, "10-subagent-inprocess.1")
		lines := loadLines(t, path)
		ids := sessionIDs(lines)
		if len(ids) != 2 {
			t.Fatalf("sessions %v", ids)
		}
		outer, nested := ids[0], ids[1]
		r := replay(t, path, replayOptions{})
		var starts []string
		for _, h := range r.hooks("pi-session-start") {
			starts = append(starts, gstr(h, "session_id"))
			if _, ok := h["parent_session_id"]; ok {
				t.Errorf("a start names a parent")
			}
		}
		if !slices.Equal(starts, []string{outer, nested}) {
			t.Errorf("session starts = %v", starts)
		}
		var prompted, called bool
		for _, rec := range r.records("pi.user_prompt") {
			prompted = prompted || rec.Attrs.str("session.id") == nested
		}
		for _, s := range r.calls() {
			called = called || s.Attrs.str("session.id") == nested
		}
		if !prompted || !called {
			t.Errorf("nested session: prompt %v, call %v", prompted, called)
		}
		for _, a := range r.everyAttrs() {
			if a.has("pi.parent_session.id") {
				t.Errorf("linked to %s", a.str("pi.parent_session.id"))
			}
		}
	})
}

// A /resume back into a session another process wrote to meanwhile: none of the other's
// entries is sent, the title is sent again as the same event, and what follows is sent.
func TestReplayResumeIntoChangedSession(t *testing.T) {
	t.Parallel()
	lines := loadLines(t, recording("1.6.1", "11-named.1"))
	end := lastIndex(lines, "session_shutdown")
	last := lines[end]
	last["event"] = map[string]any{"reason": "resume"}
	sid := gstr(last, "ctx", "sessionId")
	ctx := func(leaf string) map[string]any {
		c := map[string]any{}
		for k, v := range last["ctx"].(map[string]any) {
			c[k] = v
		}
		c["leafId"] = leaf
		return c
	}
	at := gnum(last, "t")
	leaf := gstr(last, "ctx", "leafId")
	msg := func(id, parent, role, text string, ts string) map[string]any {
		m := map[string]any{"role": role}
		if text != "" {
			m["content"] = []any{map[string]any{"type": "text", "text": text}}
		} else {
			m["content"] = []any{}
		}
		return map[string]any{"type": "message", "id": id, "parentId": parent, "timestamp": ts, "message": m}
	}
	lines = append(lines,
		line{"e": "_entries", "sessionId": sid, "entries": []any{
			msg("f0f0f0f1", leaf, "user", "FOREIGN PROMPT", "2026-11-27T15:40:00.000Z"),
			msg("f0f0f0f2", "f0f0f0f1", "assistant", "", "2026-11-27T15:40:01.000Z"),
		}},
		line{"e": "session_start", "t": at + 100, "event": map[string]any{"reason": "resume"}, "ctx": ctx("f0f0f0f2")},
		line{"e": "before_agent_start", "t": at + 200, "event": map[string]any{"messageType": "user", "prompt": "AFTER RESUME"}, "ctx": ctx("f0f0f0f2")},
		line{"e": "_entries", "sessionId": sid, "entries": []any{
			msg("a0a0a0a1", "f0f0f0f2", "user", "AFTER RESUME", "2026-11-27T15:41:00.000Z"),
			msg("a0a0a0a2", "a0a0a0a1", "assistant", "", "2026-11-27T15:41:01.000Z"),
		}},
		line{"e": "turn_end", "t": at + 300, "event": map[string]any{"messageEntryId": "a0a0a0a2", "message": map[string]any{
			"role": "assistant", "provider": "fake", "model": "m", "stopReason": "stop", "timestamp": at + 250, "durationMs": 20.0,
			"usage":   map[string]any{"input": 1.0, "output": 1.0, "cacheRead": 0.0, "cacheWrite": 0.0, "reasoning": 0.0, "cost": map[string]any{"total": 0.0, "totalFromProvider": false}},
			"content": []any{map[string]any{"type": "text", "text": "after"}}}}, "ctx": ctx("a0a0a0a2")},
		line{"e": "agent_settled", "t": at + 310, "event": map[string]any{"aborted": false}, "ctx": ctx("a0a0a0a2")},
		line{"e": "session_shutdown", "t": at + 320, "event": map[string]any{"reason": "quit"}, "ctx": ctx("a0a0a0a2")},
	)
	r := replay(t, writeLines(t, lines), replayOptions{})
	for _, a := range r.everyAttrs() {
		for _, k := range []string{"pi.call.id", "pi.entry.id", "pi.turn.id"} {
			if v := a.str(k); v == "f0f0f0f1" || v == "f0f0f0f2" {
				t.Errorf("another process's entry sent as %s", k)
			}
		}
	}
	if strings.Contains(r.allBodies(), "FOREIGN PROMPT") {
		t.Errorf("another process's prompt exported")
	}
	titles := r.records("pi.session.title")
	if len(titles) != 2 || titles[0].Attrs.str("pi.entry.id") != titles[1].Attrs.str("pi.entry.id") || titles[0].Time != titles[1].Time {
		t.Errorf("titles = %+v", titles)
	}
	c, ok := callByID(r, "a0a0a0a2")
	if !ok {
		t.Fatalf("no call after the resume")
	}
	eq(t, "turn", c.Attrs.str("pi.turn.id"), "a0a0a0a1")
	var prompted bool
	for _, p := range r.records("pi.user_prompt") {
		prompted = prompted || p.Attrs.str("pi.entry.id") == "a0a0a0a1"
	}
	if !prompted {
		t.Errorf("the prompt after the resume was not sent")
	}
}

// 24-reload: /reload is the same run; and a failed reach fences the session for good.
func TestReplayReload(t *testing.T) {
	t.Parallel()
	both(t, func(t *testing.T, v string) {
		path := recording(v, "24-reload.1")
		lines := loadLines(t, path)
		r := replay(t, path, replayOptions{})
		eq(t, "session starts", len(r.hooks("pi-session-start")), 1)
		eq(t, "session ends", len(r.hooks("pi-session-end")), 1)
		eq(t, "calls", len(r.calls()), len(events(lines, "turn_end")))
		eq(t, "tools", len(r.tools()), len(events(lines, "tool_execution_end")))
		eq(t, "prompts", len(r.records("pi.user_prompt")), 2)

		f := replay(t, path, replayOptions{FailReach: true})
		var fence, reaches int
		for _, h := range f.hooks("pi-tool-reach") {
			reaches++
			if gstr(h, "command") == "eval $TERMA_REACH_FAILED" {
				fence++
			}
		}
		if fence != 1 || reaches != 2 {
			t.Errorf("reach calls %d, fence calls %d; want the failed reach and one fence", reaches, fence)
		}
		if len(f.Spans) != 0 || len(f.Records) != 0 {
			t.Errorf("a fenced session exported %d spans and %d records", len(f.Spans), len(f.Records))
		}
		eq(t, "fenced edits", len(f.hooks("pi-file-edit")), 0)
		eq(t, "fenced prompts", len(f.hooks("pi-prompt")), 1)
		eq(t, "fenced session starts", len(f.hooks("pi-session-start")), 1)
	})
}

// 11-*: names, a missing price, cache and reasoning tokens.
func TestReplayNamesAndUsage(t *testing.T) {
	t.Parallel()
	titleOf := func(t *testing.T, lines []line) map[string]any {
		for _, e := range entries(lines) {
			if gstr(e.Entry, "type") == "session_info" {
				return e.Entry
			}
		}
		t.Fatalf("recording: no session_info")
		return nil
	}
	t.Run("11-no-session", func(t *testing.T) {
		t.Parallel()
		path := recording("1.6.1", "11-no-session.1")
		lines := loadLines(t, path)
		r := replay(t, path, replayOptions{})
		e := titleOf(t, lines)
		titles := r.records("pi.session.title")
		if len(titles) != 1 {
			t.Fatalf("titles = %d", len(titles))
		}
		eq(t, "entry", titles[0].Attrs.str("pi.entry.id"), gstr(e, "id"))
		eq(t, "body", titles[0].Body, gstr(e, "name"))
		eq(t, "time", titles[0].Time, isoNanos(t, gstr(e, "timestamp")))
	})
	t.Run("11-named", func(t *testing.T) {
		t.Parallel()
		both(t, func(t *testing.T, v string) {
			p1, p2 := recording(v, "11-named.1"), recording(v, "11-named.2")
			r1 := replay(t, p1, replayOptions{})
			r2 := replay(t, p2, replayOptions{})
			t1, t2 := r1.records("pi.session.title"), r2.records("pi.session.title")
			if len(t1) != 1 || len(t2) != 1 {
				t.Fatalf("titles %d, %d", len(t1), len(t2))
			}
			eq(t, "entry", t2[0].Attrs.str("pi.entry.id"), t1[0].Attrs.str("pi.entry.id"))
			eq(t, "body", t2[0].Body, t1[0].Body)
			eq(t, "time", t2[0].Time, t1[0].Time)
			eq(t, "entry is the name's", t1[0].Attrs.str("pi.entry.id"), gstr(titleOf(t, loadLines(t, p1)), "id"))
			lines2 := loadLines(t, p2)
			sid := sessionIDs(lines2)[0]
			held := entriesBefore(lines2, sid, indexOf(lines2, func(l line) bool { return l["e"] == "session_start" }))
			for _, p := range r2.records("pi.user_prompt") {
				if held[p.Attrs.str("pi.entry.id")] {
					t.Errorf("the resumed process re-sent prompt %s", p.Attrs.str("pi.entry.id"))
				}
			}
			if len(r2.records("pi.user_prompt")) != 1 {
				t.Errorf("prompts in the resumed process = %d", len(r2.records("pi.user_prompt")))
			}
		})
	})
	t.Run("11-no-cost-model", func(t *testing.T) {
		t.Parallel()
		r := replay(t, recording("1.6.1", "11-no-cost-model.1"), replayOptions{})
		calls := r.calls()
		if len(calls) == 0 {
			t.Fatal("no calls")
		}
		for _, c := range calls {
			cost, ok := c.Attrs.double("gen_ai.usage.total_cost")
			if !ok || cost != 0 {
				t.Errorf("cost = %v (double %v)", cost, ok)
			}
			if fp, ok := c.Attrs.boolean("pi.cost.from_provider"); !ok || fp {
				t.Errorf("from_provider = %v (%v)", fp, ok)
			}
		}
	})
	t.Run("11-cache-and-reasoning", func(t *testing.T) {
		t.Parallel()
		r := replay(t, recording("1.6.1", "11-cache-and-reasoning.1"), replayOptions{})
		var found bool
		for _, c := range r.calls() {
			if n, _ := c.Attrs.int("gen_ai.usage.reasoning.output_tokens"); n != 120 {
				continue
			}
			found = true
			eq(t, "input", intAttr(t, c.Attrs, "gen_ai.usage.input_tokens"), 400)
			eq(t, "cache read", intAttr(t, c.Attrs, "gen_ai.usage.cache_read.input_tokens"), 600)
			eq(t, "output", intAttr(t, c.Attrs, "gen_ai.usage.output_tokens"), 300)
			eq(t, "thinking level", c.Attrs.str("pi.thinking_level"), "high")
		}
		if !found {
			t.Errorf("no call with 120 reasoning tokens")
		}
	})
}

// Prompts and the messages an extension puts into the conversation, each in its turn.
func TestReplayCustomMessages(t *testing.T) {
	t.Parallel()
	users := func(lines []line) []string {
		var out []string
		for _, e := range entries(lines) {
			if gstr(e.Entry, "type") == "message" && gstr(e.Entry, "message", "role") == "user" {
				out = append(out, gstr(e.Entry, "id"))
			}
		}
		return out
	}
	customs := func(lines []line) []map[string]any {
		var out []map[string]any
		for _, e := range entries(lines) {
			if gstr(e.Entry, "type") == "custom_message" {
				out = append(out, e.Entry)
			}
		}
		return out
	}
	t.Run("02-custom-message", func(t *testing.T) {
		t.Parallel()
		path := recording("1.6.1", "02-custom-message.1")
		lines := loadLines(t, path)
		r := replay(t, path, replayOptions{})
		cm := customs(lines)[0]
		id := gstr(cm, "id")
		recs := r.records("pi.custom_message")
		if len(recs) != 1 {
			t.Fatalf("custom messages = %d", len(recs))
		}
		eq(t, "entry", recs[0].Attrs.str("pi.entry.id"), id)
		eq(t, "turn", recs[0].Attrs.str("pi.turn.id"), id)
		eq(t, "body", recs[0].Body, textOfContent(cm["content"]))
		eq(t, "length", intAttr(t, recs[0].Attrs, "pi.prompt.length"), jsLen(textOfContent(cm["content"])))
		first := events(lines, "turn_end")[0]
		c, _ := callByID(r, gstr(first, "event", "messageEntryId"))
		eq(t, "the run's call turn", c.Attrs.str("pi.turn.id"), id)
		prompts := r.records("pi.user_prompt")
		if len(prompts) != 1 || prompts[0].Attrs.str("pi.turn.id") != prompts[0].Attrs.str("pi.entry.id") {
			t.Errorf("prompts = %+v", prompts)
		}
	})
	t.Run("20-inject", func(t *testing.T) {
		t.Parallel()
		path := recording("1.6.1", "20-inject.1")
		lines := loadLines(t, path)
		r := replay(t, path, replayOptions{})
		user := users(lines)[0]
		recs := r.records("pi.custom_message")
		if len(recs) != 1 || len(r.records("pi.user_prompt")) != 1 {
			t.Fatalf("custom messages %d, prompts %d", len(recs), len(r.records("pi.user_prompt")))
		}
		eq(t, "custom turn", recs[0].Attrs.str("pi.turn.id"), user)
		eq(t, "prompt turn", r.records("pi.user_prompt")[0].Attrs.str("pi.turn.id"), user)
		for _, c := range r.calls() {
			eq(t, "call turn", c.Attrs.str("pi.turn.id"), user)
		}
	})
	t.Run("21-queued-pair", func(t *testing.T) {
		t.Parallel()
		path := recording("1.6.1", "21-queued-pair.1")
		lines := loadLines(t, path)
		r := replay(t, path, replayOptions{})
		us := users(lines)
		prompts := r.records("pi.user_prompt")
		if len(us) != 3 || len(prompts) != 3 {
			t.Fatalf("users %d, prompts %d", len(us), len(prompts))
		}
		for i, p := range prompts {
			eq(t, "prompt entry", p.Attrs.str("pi.entry.id"), us[i])
			eq(t, "prompt turn", p.Attrs.str("pi.turn.id"), us[i])
		}
		last := events(lines, "turn_end")
		c, _ := callByID(r, gstr(last[len(last)-1], "event", "messageEntryId"))
		eq(t, "last call turn", c.Attrs.str("pi.turn.id"), us[2])
	})
	t.Run("22-clone-after-custom", func(t *testing.T) {
		t.Parallel()
		path := recording("1.6.1", "22-clone-after-custom.1")
		lines := loadLines(t, path)
		ids := sessionIDs(lines)
		r := replay(t, path, replayOptions{})
		var source, clone int
		for _, rec := range r.records("pi.custom_message") {
			switch rec.Attrs.str("session.id") {
			case ids[0]:
				source++
			case ids[1]:
				clone++
			}
		}
		if source != 1 || clone != 0 {
			t.Errorf("custom messages: source %d, clone %d", source, clone)
		}
	})
}

// The host check: the extension records only in Pi 1.5.0 or later.
func TestReplayHostCheck(t *testing.T) {
	t.Parallel()
	path := recording("1.6.1", "01-print-baseline.1")
	omp := []string{"@oh-my-pi/pi-utils"}
	for _, c := range []struct {
		name    string
		o       replayOptions
		version string
	}{
		{"inert template", replayOptions{Inert: true}, ""},
		{"no Pi module", replayOptions{NoHost: true}, ""},
		{"Pi 1.4.9", replayOptions{Host: map[string]any{"VERSION": "1.4.9", "CONFIG_DIR_NAME": ".pi"}}, ""},
		{"inside OMP", replayOptions{Host: map[string]any{"VERSION": "19.1.1", "CONFIG_DIR_NAME": ".omp"}, Modules: omp}, ""},
		{"no config dir, OMP's module", replayOptions{Host: map[string]any{"VERSION": "1.6.1"}, Modules: omp}, ""},
		{"Pi 1.5.0", replayOptions{Host: map[string]any{"VERSION": "1.5.0", "CONFIG_DIR_NAME": ".pi"}}, "1.5.0"},
		{"Pi 1.6.1", replayOptions{Host: map[string]any{"VERSION": "1.6.1", "CONFIG_DIR_NAME": ".pi"}}, "1.6.1"},
		{"Pi with a stray OMP module", replayOptions{Host: map[string]any{"VERSION": "1.6.1", "CONFIG_DIR_NAME": ".pi"}, Modules: omp}, "1.6.1"},
		{"no config dir, no OMP", replayOptions{Host: map[string]any{"VERSION": "1.7.0"}}, "1.7.0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := replay(t, path, c.o)
			if c.version == "" {
				if r.Registered != 0 || len(r.Posts) != 0 || len(r.Hooks) != 0 {
					t.Errorf("registered %d, posts %d, hooks %d; want none", r.Registered, len(r.Posts), len(r.Hooks))
				}
				return
			}
			if r.Registered == 0 || len(r.Spans) == 0 || len(r.Hooks) == 0 {
				t.Fatalf("registered %d, spans %d, hooks %d", r.Registered, len(r.Spans), len(r.Hooks))
			}
			eq(t, "pi.version", r.Spans[0].Attrs.str("pi.version"), c.version)
		})
	}
}

// A post the relay does not take is sent again, the same bytes.
func TestReplayRetrySendsTheSameBytes(t *testing.T) {
	t.Parallel()
	r := replay(t, recording("1.6.1", "01-print-baseline.1"), replayOptions{FailPosts: 1})
	if len(r.Posts) < 2 || r.Posts[0].Status != 503 {
		t.Fatalf("posts %+v", r.Posts)
	}
	eq(t, "retried path", r.Posts[1].Path, r.Posts[0].Path)
	if r.Posts[1].Body != r.Posts[0].Body {
		t.Errorf("the retry sent different bytes")
	}
}

// The content switches in CONFIG.
func TestReplayContentSwitches(t *testing.T) {
	t.Parallel()
	base := recording("1.6.1", "01-print-baseline.1")
	t.Run("no prompts", func(t *testing.T) {
		t.Parallel()
		r := replay(t, base, replayOptions{NoPrompts: true})
		p := r.records("pi.user_prompt")[0]
		if p.Body != "" || !p.Attrs.has("pi.prompt.length") {
			t.Errorf("prompt body %q, length %v", p.Body, p.Attrs.has("pi.prompt.length"))
		}
		var lengths int
		for _, c := range r.calls() {
			if c.Attrs.has("gen_ai.completion") {
				t.Errorf("a reply was sent")
			}
			if c.Attrs.has("pi.completion.length") {
				lengths++
			}
		}
		if lengths == 0 {
			t.Errorf("the reply's length went too")
		}
		if !r.tool("write")[0].Attrs.has("gen_ai.tool.call.arguments") {
			t.Errorf("tool content went with prompts")
		}
		named := replay(t, recording("1.6.1", "11-named.1"), replayOptions{NoPrompts: true})
		if ts := named.records("pi.session.title"); len(ts) != 1 || ts[0].Body != "" {
			t.Errorf("titles %+v", ts)
		}
	})
	t.Run("no tool content", func(t *testing.T) {
		t.Parallel()
		r := replay(t, base, replayOptions{NoToolContent: true})
		for _, s := range r.tools() {
			for _, k := range []string{"gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "pi.tool.files"} {
				if s.Attrs.has(k) {
					t.Errorf("%s carries %s", s.Name, k)
				}
			}
		}
		if r.records("pi.user_prompt")[0].Body == "" {
			t.Errorf("the prompt went with tool content")
		}
	})
	for _, o := range []replayOptions{{NoPrompts: true}, {NoToolContent: true}} {
		t.Run(fmt.Sprintf("either off %+v", o), func(t *testing.T) {
			t.Parallel()
			c := replay(t, recording("1.6.1", "02-custom-message.1"), o)
			if rs := c.records("pi.custom_message"); len(rs) != 1 || rs[0].Body != "" || !rs[0].Attrs.has("pi.prompt.length") {
				t.Errorf("custom messages %+v", rs)
			}
			e := replay(t, recording("1.6.1", "07-error-400.1"), o)
			if e.calls()[0].Attrs.has("pi.error.message") {
				t.Errorf("an error message was sent")
			}
			s := replay(t, recording("1.6.1", "23-two-compactions.1"), o)
			for _, sp := range s.chats() {
				if sp.Attrs.has("pi.compaction.summary") {
					t.Errorf("a compaction summary was sent")
				}
			}
		})
	}
}

// oneSession is a recording of one Pi 1.6.1 process in /home/user/work/repo: the lines
// given (an event as {"e", "event"}, entries as {"e": "_entries", "entries"}), then a
// shutdown. It starts with a session_start unless the first line is an event of its own.
func oneSession(lines ...line) []line {
	const sid = "01a21daf-0000-7000-8000-0000000000f1"
	ctx := map[string]any{"sessionId": sid, "leafId": nil, "mode": "print", "modelProvider": "fake", "modelId": "m", "cwd": "/home/user/work/repo", "oauth": false}
	out := []line{
		{"e": "_process", "pid": 1.0, "ppid": 1.0, "argv": []any{}, "version": "1.6.1", "inheritedParent": nil},
		{"e": "_session", "sessionId": sid, "sessionFile": "", "header": map[string]any{"type": "session", "id": sid, "cwd": "/home/user/work/repo"}},
	}
	if len(lines) == 0 || lines[0]["e"] != "session_start" {
		lines = append([]line{{"e": "session_start", "event": map[string]any{"reason": "startup"}}}, lines...)
	}
	at := 1795793600000.0
	for _, l := range append(lines, line{"e": "session_shutdown", "event": map[string]any{"reason": "quit"}}) {
		if l["e"] == "_entries" {
			l["sessionId"] = sid
		} else {
			at++
			l["t"], l["ctx"] = at, ctx
		}
		out = append(out, l)
	}
	return out
}

// A tool call and the turn that issued it: an assistant entry, the call's start and end with
// result, and its turn_end.
func toolTurn(name string, args, result map[string]any) []line {
	return []line{
		{"e": "_entries", "entries": []any{map[string]any{"type": "message", "id": "a0000001", "parentId": nil, "timestamp": "2026-11-27T15:40:00.000Z",
			"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "toolCall", "id": "c1", "name": name}}}}}},
		{"e": "tool_execution_start", "event": map[string]any{"toolCallId": "c1", "toolName": name, "args": args}},
		{"e": "tool_call", "event": map[string]any{"toolCallId": "c1", "toolName": name, "input": args}},
		{"e": "tool_execution_end", "event": map[string]any{"toolCallId": "c1", "toolName": name, "result": result, "isError": false, "durationMs": 1.0}},
		{"e": "turn_end", "event": map[string]any{"messageEntryId": "a0000001", "message": map[string]any{"role": "assistant", "provider": "fake", "model": "m",
			"stopReason": "toolUse", "timestamp": 1795793600000.0, "durationMs": 1.0, "content": []any{}}}},
	}
}

// The fixes after the first review, each on a synthetic recording.
func TestReplayReviewFixes(t *testing.T) {
	t.Parallel()
	text := func(s string) map[string]any {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}
	}
	t.Run("a path Pi cannot open is no failed reach", func(t *testing.T) {
		t.Parallel()
		r := replay(t, writeLines(t, oneSession(toolTurn("read", map[string]any{"path": "file://host/x"}, text("no such file"))...)), replayOptions{})
		if n := len(r.hooks("pi-tool-reach")); n != 0 {
			t.Errorf("reach calls = %d", n)
		}
		if len(r.tool("read")) != 1 {
			t.Errorf("the session stopped exporting: tool spans %d", len(r.tools()))
		}
	})
	t.Run("only an MCP tool names a server", func(t *testing.T) {
		t.Parallel()
		result := text("done")
		result["details"] = map[string]any{"server": "s", "tool": "t"}
		r := replay(t, writeLines(t, oneSession(toolTurn("custom", map[string]any{}, result)...)), replayOptions{})
		if s := r.tool("custom"); len(s) != 1 || s[0].Attrs.has("pi.mcp.server") || s[0].Attrs.has("pi.mcp.tool") {
			t.Errorf("spans %+v", s)
		}
	})
	attach := func(prompt string) []line {
		return oneSession(line{"e": "before_agent_start", "event": map[string]any{"messageType": "user", "prompt": prompt}},
			line{"e": "_entries", "entries": []any{map[string]any{"type": "message", "id": "u0000001", "parentId": nil, "timestamp": "2026-11-27T15:40:00.000Z",
				"message": map[string]any{"role": "user", "content": prompt}}}},
			line{"e": "turn_end", "event": map[string]any{"messageEntryId": "u0000001", "message": map[string]any{"role": "assistant", "provider": "fake", "model": "m",
				"stopReason": "stop", "timestamp": 1795793600000.0, "durationMs": 1.0, "content": []any{}}}})
	}
	const files = `<file name="/other/a.go">x</file> <file name="/other/b.go">y</file> <file name="/other/a.go">x</file> go`
	t.Run("attached files are reach-checked", func(t *testing.T) {
		t.Parallel()
		r := replay(t, writeLines(t, attach(files)), replayOptions{})
		var order []string
		for _, h := range r.Hooks {
			order = append(order, h.Event)
		}
		reaches := r.hooks("pi-tool-reach")
		if len(reaches) != 1 || !slices.Equal(toStrings(reaches[0]["paths"]), []string{"/other/a.go", "/other/b.go"}) ||
			slices.Index(order, "pi-prompt") > slices.Index(order, "pi-tool-reach") {
			t.Errorf("hooks %v, reaches %v", order, reaches)
		}
		f := replay(t, writeLines(t, attach(files)), replayOptions{FailReach: true})
		if len(f.records("pi.user_prompt")) != 0 || len(f.Spans) != 0 {
			t.Errorf("a fenced session exported its prompt")
		}
		if n := len(replay(t, writeLines(t, attach("no files")), replayOptions{}).hooks("pi-tool-reach")); n != 0 {
			t.Errorf("a prompt with no file: %d reach calls", n)
		}
	})
	t.Run("a reload of an unseen session is a start", func(t *testing.T) {
		t.Parallel()
		lines := oneSession(line{"e": "session_start", "event": map[string]any{"reason": "reload"}})
		r := replay(t, writeLines(t, lines), replayOptions{})
		sid := gstr(lines[1], "sessionId")
		if n := len(r.hooks("pi-session-start")); n != 1 || r.EnvParent == nil || *r.EnvParent != fmt.Sprintf("%d:%s", r.PID, sid) {
			t.Errorf("starts %d, TERMA_PI_PARENT %v", n, r.EnvParent)
		}
	})
	t.Run("a cleared name is no title", func(t *testing.T) {
		t.Parallel()
		named := func(id, name string) map[string]any {
			return map[string]any{"type": "session_info", "id": id, "parentId": nil, "timestamp": "2026-11-27T15:39:00.000Z", "name": name}
		}
		lines := oneSession()
		lines = insertAt(lines, 2, line{"e": "_entries", "sessionId": gstr(lines[1], "sessionId"), "entries": []any{named("i0000001", "Old"), named("i0000002", "")}})
		if n := len(replay(t, writeLines(t, lines), replayOptions{}).records("pi.session.title")); n != 0 {
			t.Errorf("titles %d", n)
		}
	})
}
