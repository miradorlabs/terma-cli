package refdiff

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Comparing two builds' filesystem sections: what each did on the file system and with
// sockets, traced (test/e2e/filetrace.go), by platform. Only a platform both snapshots have is
// compared, and of it only what a build does, never what a run measured: a path's ops (not when
// it was touched, or by which helper), the programs started and when, the programs looked for,
// what was done to each decoy, the sockets listened on and connected to, and on macOS the XPC
// services, launch items, TCC changes and task ports. Counts are the run's. A path or socket
// whose name is the run's own (a lock file, a temporary name, an id) that one build touched and
// the other did not is no change, and neither is one the builds either side of a build have and
// its run missed (unobserved, set by refgen). A macOS trace's paths are not compared (see
// diffFilesystem).

// fsVolatile matches a name a run makes for itself.
var fsVolatile = regexp.MustCompile(`<(?:uuid|hash|id|time|date)>|\.lock\b|\.tmp\b|~$|-(?:journal|wal|shm)$|\.swp$`)

func init() {
	for _, s := range []string{"paths", "exec", "ops", "phases", "decoys", "sockets", "listening", "connections", "unix",
		"dns", "macos", "xpc", "launch_items", "tcc", "task_ports", "lookups", "by", "role", "linux/amd64", "linux/arm64",
		"darwin/arm64", "darwin/amd64"} {
		genericSegments[s] = true
	}
}

// diffFilesystem appends the changes from a's filesystem section to b's.
func diffFilesystem(a, b any, out *[]Change) {
	am, _ := a.(map[string]any)
	bm, _ := b.(map[string]any)
	for _, plat := range sortedKeys(keysOf(am)) {
		x, _ := am[plat].(map[string]any)
		y, _ := bm[plat].(map[string]any)
		if x == nil || y == nil {
			continue // a platform one build was not traced on
		}
		unobserved := map[string]bool{}
		for _, side := range []map[string]any{x, y} {
			u, _ := side["unobserved"].(map[string]any)
			for _, list := range u {
				for _, k := range strs(list) {
					unobserved[k] = true
				}
			}
		}
		at := func(part ...string) []string { return append([]string{"filesystem", plat}, part...) }
		keyed := func(part []string, xs, ys map[string]any, volatileNames bool, inner func(k string, xv, yv map[string]any)) {
			for _, k := range sortedKeys(fsUnion(xs, ys)) {
				xv, inX := xs[k]
				yv, inY := ys[k]
				switch {
				case inX && inY:
					if inner != nil {
						xm, _ := xv.(map[string]any)
						ym, _ := yv.(map[string]any)
						inner(k, xm, ym)
					}
				case unobserved[k] || volatileNames && fsVolatile.MatchString(k):
				case inY:
					*out = append(*out, Change{Op: "added", Path: append(slices.Clone(part), k), After: yv})
				default:
					*out = append(*out, Change{Op: "removed", Path: append(slices.Clone(part), k), Before: xv})
				}
			}
		}
		words := func(path []string, xs, ys []string) {
			for _, w := range ys {
				if !slices.Contains(xs, w) {
					*out = append(*out, Change{Op: "added", Path: append(slices.Clone(path), w), After: w})
				}
			}
			for _, w := range xs {
				if !slices.Contains(ys, w) {
					*out = append(*out, Change{Op: "removed", Path: append(slices.Clone(path), w), Before: w})
				}
			}
		}
		// Endpoint Security logs every path a build resolves, and what a build's background work
		// (Claude Code's cleanup walk) reached before it exited is a race: the same build traced
		// twice lists different paths. Its paths are each build's own to read, never compared;
		// its programs, sockets, decoys and services are.
		if asString(x["tracer"]) != "eslogger" && asString(y["tracer"]) != "eslogger" {
			keyed(at("paths"), sub(x, "paths"), sub(y, "paths"), true, func(k string, xv, yv map[string]any) {
				// What a run's own temporary file had done to it is the run's.
				if !fsVolatile.MatchString(k) {
					words(at("paths", k, "ops"), strs(xv["ops"]), strs(yv["ops"]))
				}
			})
		}
		before := len(*out)
		keyed(at("exec"), withoutTermaHooks(sub(x, "exec")), withoutTermaHooks(sub(y, "exec")), false, func(k string, xv, yv map[string]any) {
			words(at("exec", k, "phases"), strs(xv["phases"]), strs(yv["phases"]))
			// Who starts it, and whose doing it is (the build's own, the tool call's): a build
			// traced before roles were recorded has none, which is no change. Its parents and
			// purpose are presentation: never compared.
			for _, field := range []string{"by", "role"} {
				if xb, yb := xv[field], yv[field]; xb != nil && yb != nil && xb != yb {
					*out = append(*out, Change{Op: "changed", Path: at("exec", k, field), Before: xb, After: yb})
				}
			}
		})
		// A command gone and one new that run the same program with most of the same arguments
		// are one command changed: a script's body, a flag added.
		var commands, inner []Change
		for _, c := range (*out)[before:] {
			if len(c.Path) == 4 {
				commands = append(commands, c)
			} else {
				inner = append(inner, c)
			}
		}
		*out = append(append((*out)[:before], inner...), pairCommands(commands)...)
		// What a macOS build does after it exits (a session-end hook, a last git status) reaches
		// the trace or not: eslogger's last events can still be on their way when the trace stops.
		// A command that ran only at exit, or a command's exit phase, is not compared there, nor
		// who started such a command or whose doing it was.
		if asString(x["tracer"]) == "eslogger" || asString(y["tracer"]) == "eslogger" {
			atExit := func(v any) bool {
				m, _ := v.(map[string]any)
				ph := strs(m["phases"])
				return len(ph) == 1 && ph[0] == "exit"
			}
			xe, ye := sub(x, "exec"), sub(y, "exec")
			*out = append((*out)[:before], slices.DeleteFunc((*out)[before:], func(c Change) bool {
				if len(c.Path) == 6 && c.Path[4] == "phases" {
					return c.Path[5] == "exit"
				}
				if len(c.Path) == 5 {
					return atExit(xe[c.Path[3]]) || atExit(ye[c.Path[3]])
				}
				return len(c.Path) == 4 && c.Op != "changed" && (atExit(c.Before) || atExit(c.After))
			})...)
		}
		words(at("lookups"), strs(x["lookups"]), strs(y["lookups"]))
		xd, yd := sub(x, "decoys"), sub(y, "decoys")
		for _, k := range sortedKeys(keysOf(yd)) {
			// A decoy one build had and the other did not was not planted then: no change.
			if before, ok := xd[k]; ok && before != yd[k] {
				*out = append(*out, Change{Op: "changed", Path: at("decoys", k), Before: before, After: yd[k]})
			}
		}
		xs, ys := sub(x, "sockets"), sub(y, "sockets")
		for _, part := range []string{"listening", "connections", "unix"} {
			keyed(at("sockets", part), sub(xs, part), sub(ys, part), part == "unix", nil)
		}
		words(at("sockets", "dns"), strs(xs["dns"]), strs(ys["dns"]))
		xm, ym := sub(x, "macos"), sub(y, "macos")
		if xm != nil && ym != nil {
			for _, part := range []string{"xpc", "launch_items", "tcc", "task_ports"} {
				keyed(at("macos", part), sub(xm, part), sub(ym, part), false, nil)
			}
		}
	}
}

// withoutTermaHooks is an exec list without terma's hooks: a trace from before the files
// profile was a clean room ran with them (`terma hook …`, and the shell each ran in, `sh -c [ -x
// 'terma' ] && 'terma' hook …`). They were the scenario's, not the build's, so a build traced
// with them and one traced without differ in nothing there.
func withoutTermaHooks(exec map[string]any) map[string]any {
	out := make(map[string]any, len(exec))
	for k, v := range exec {
		if !strings.HasPrefix(k, "terma hook ") && !strings.Contains(k, "'terma' hook ") {
			out[k] = v
		}
	}
	return out
}

func sub(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}

func strs(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, s := range x {
			if str, ok := s.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

func keysOf(m map[string]any) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

func fsUnion(a, b map[string]any) map[string]bool {
	out := keysOf(a)
	for k := range b {
		out[k] = true
	}
	return out
}

// The ops of a path, the most telling first, and how a title says each.
var (
	fsOpRank = []string{"write", "create", "delete", "rename", "denied", "read", "list", "chdir", "stat", "probe"}
	fsVerb   = map[string]string{"write": "writes", "create": "creates", "delete": "deletes", "rename": "renames",
		"denied": "is denied", "read": "reads", "list": "lists", "chdir": "works in", "stat": "checks", "probe": "looks for"}
	// fsOwn are the folders a harness writes as its own: no news when it writes there.
	fsOwn = []string{"<claude-config>/", "<codex-home>/", "<tmp>/", "<repo>/", "<sandbox>/", "<terma-config>/"}
	// fsSecurityXPC are the services that hold or grant secrets and permissions.
	fsSecurityXPC = []string{"com.apple.securityd", "com.apple.SecurityServer", "com.apple.tccd", "com.apple.security.", "com.apple.trustd", "com.apple.authd"}
)

// strongest is the most telling of a path's ops.
func strongest(ops []string) string {
	for _, op := range fsOpRank {
		if slices.Contains(ops, op) {
			return op
		}
	}
	return "stat"
}

// tilde writes the home as ~.
func tilde(path string) string {
	if rest, ok := strings.CutPrefix(path, "<home>"); ok {
		return "~" + rest
	}
	return path
}

func fsOps(v any) []string {
	m, _ := v.(map[string]any)
	return strs(m["ops"])
}

// filesystemImpact is what a filesystem change means for someone relying on the harness.
func filesystemImpact(c *Change) string {
	p := c.Path
	if len(p) < 4 {
		return "informational"
	}
	part := p[2]
	switch {
	case part == "decoys":
		after, _ := c.After.(string)
		if decoyRank(after) > decoyRank("stat") && decoyRank(after) > decoyRank(asString(c.Before)) {
			return "notable"
		}
	case part == "paths" && c.Op != "removed":
		op := ""
		switch len(p) {
		case 4:
			op = strongest(fsOps(c.After))
		case 6:
			op = p[5]
		}
		writes := op == "write" || op == "create" || op == "delete" || op == "rename"
		own := slices.ContainsFunc(fsOwn, func(d string) bool { return strings.HasPrefix(p[3], d) })
		if writes && !own {
			return "notable" // a file of the user's that is not the harness's own
		}
	case part == "exec" && len(p) == 4 && c.Op == "added":
		return "notable"
	case part == "exec" && len(p) == 5 && p[4] == "role" && c.After != "tool":
		return "notable" // the build now runs on its own what was the tool call's
	case part == "lookups" && c.Op == "added":
		return "notable"
	case part == "sockets" && c.Op == "added" && len(p) == 5:
		switch p[3] {
		case "listening", "dns":
			return "notable"
		case "connections":
			if m, _ := c.After.(map[string]any); m["role"] == nil || m["role"] == "" || m["role"] == "dns" {
				return "notable" // anything but the run's own endpoints
			}
		case "unix":
			if m, _ := c.After.(map[string]any); m["role"] != "nscd" && m["role"] != "syslog" {
				return "notable"
			}
		}
	case part == "macos" && c.Op == "added" && len(p) == 5:
		if p[3] != "xpc" || slices.ContainsFunc(fsSecurityXPC, func(s string) bool { return strings.HasPrefix(p[4], s) }) {
			return "notable"
		}
	}
	return "informational"
}

func asString(v any) string { s, _ := v.(string); return s }

// roleWords say an exec entry's role in a title.
var roleWords = map[string]string{"harness": "the build's own doing", "tool": "part of the tool call",
	"both": "both the build's own and the tool call's"}

var decoyOrder = []string{"not touched", "stat", "opened", "read", "connected", "written", "deleted"}

func decoyRank(level string) int { return slices.Index(decoyOrder, level) }

// filesystemTitle is a filesystem change said in a line.
func filesystemTitle(c *Change) string {
	p := c.Path
	if len(p) < 4 {
		return strings.Join(p, ".") + " " + c.Op
	}
	plat, part, key := p[1], p[2], p[3]
	on := " (" + plat + ")"
	now, no := "Now ", "No longer "
	switch part {
	case "paths":
		switch {
		case len(p) == 4 && c.Op == "added":
			return now + fsVerb[strongest(fsOps(c.After))] + " " + tilde(key) + on
		case len(p) == 4:
			return no + fsVerb[strongest(fsOps(c.Before))] + " " + tilde(key) + on
		case len(p) == 6 && c.Op == "added":
			return now + fsVerb[p[5]] + " " + tilde(key) + on
		case len(p) == 6:
			return no + fsVerb[p[5]] + " " + tilde(key) + on
		}
	case "exec":
		cmd := "`" + shortCommand(key) + "`"
		switch {
		case len(p) == 4 && c.Op == "added":
			m, _ := c.After.(map[string]any)
			return "New command at " + strings.Join(strs(m["phases"]), " and ") + ": " + cmd + on
		case len(p) == 4 && c.Op == "changed":
			stats := ""
			if c.Stats != nil {
				stats = fmt.Sprintf(" (+%d −%d chars)", c.Stats.AddedChars, c.Stats.RemovedChars)
			}
			return "Command changed: " + cmd + stats + on
		case len(p) == 4:
			return "Command no longer run: " + cmd + on
		case len(p) == 6 && c.Op == "added":
			return cmd + " now also runs at " + p[5] + on
		case len(p) == 6:
			return cmd + " no longer runs at " + p[5] + on
		case len(p) == 5 && p[4] == "by":
			return cmd + " now started by " + asString(c.After) + " (was " + asString(c.Before) + ")" + on
		case len(p) == 5 && p[4] == "role":
			return cmd + " now runs as " + roleWords[asString(c.After)] + " (was " + roleWords[asString(c.Before)] + ")" + on
		}
	case "lookups":
		if c.Op == "added" {
			return now + "looks for the program " + key + " on the PATH" + on
		}
		return no + "looks for the program " + key + " on the PATH" + on
	case "decoys":
		after := asString(c.After)
		switch after {
		case "not touched":
			return no + "touches " + tilde(key) + " (a planted decoy)" + on
		case "connected":
			if strings.Contains(key, "docker") {
				return "Connects to the decoy Docker socket " + tilde(key) + on
			}
			return "Connects to the decoy ssh-agent socket " + tilde(key) + on
		}
		verb := map[string]string{"stat": "Checks", "opened": "Opens", "read": "Reads", "written": "Writes", "deleted": "Deletes"}[after]
		if decoyRank(after) < decoyRank(asString(c.Before)) {
			return "Touches " + tilde(key) + " less: " + after + " (was " + asString(c.Before) + "; a planted decoy)" + on
		}
		return verb + " " + tilde(key) + " (a planted decoy)" + on
	case "sockets":
		if len(p) < 5 {
			break
		}
		name := p[4]
		switch key {
		case "listening":
			proto, addr, _ := strings.Cut(name, " ")
			addr = strings.Replace(addr, ":<ephemeral>", ", an ephemeral port", 1)
			if c.Op == "added" {
				return now + "listens on " + addr + " (" + proto + ")" + on
			}
			return no + "listens on " + addr + " (" + proto + ")" + on
		case "connections":
			proto, to, _ := strings.Cut(name, " ")
			if c.Op == "added" {
				return now + "connects to " + to + " (" + proto + ")" + on
			}
			return no + "connects to " + to + " (" + proto + ")" + on
		case "unix":
			role := ""
			m, _ := c.After.(map[string]any)
			if c.Op == "removed" {
				m, _ = c.Before.(map[string]any)
			}
			if r := asString(m["role"]); r != "" {
				role = " (" + r + ")"
			}
			verb := "connects to"
			if ops := strs(m["ops"]); slices.Contains(ops, "bind") && !slices.Contains(ops, "connect") {
				verb = "listens on"
			}
			if c.Op == "added" {
				return now + verb + " the Unix socket " + tilde(name) + role + on
			}
			return no + verb + " the Unix socket " + tilde(name) + role + on
		case "dns":
			if c.Op == "added" {
				return now + "looks up " + name + on
			}
			return no + "looks up " + name + on
		}
	case "macos":
		if len(p) < 5 {
			break
		}
		name := tilde(p[4])
		added := c.Op == "added"
		switch key {
		case "xpc":
			if added {
				return now + "connects to the XPC service " + name + on
			}
			return no + "connects to the XPC service " + name + on
		case "launch_items":
			if added {
				return "Adds a launch item (runs at login): " + name + on
			}
			return no + "adds the launch item " + name + on
		case "tcc":
			if added {
				return "Changes a privacy permission (TCC): " + name + on
			}
			return no + "changes the privacy permission " + name + on
		case "task_ports":
			if p[4] == "<processes>" && added {
				return "A program it starts reads every process's task port (as ps does)" + on
			}
			if added {
				return "Takes the task port of another process: " + name + on
			}
			return no + "takes the task port of " + name + on
		}
	}
	return "Files " + strings.Join(p[2:], ".") + " " + c.Op + on
}

// isDecoy reports a change of what a build did to a planted decoy.
func isDecoy(c Change) bool {
	return c.Category == "filesystem" && len(c.Path) > 2 && c.Path[2] == "decoys"
}

// pairCommands makes each command removed and the added one most like it (the same program,
// at least half the same arguments) one change, its command line's text diff.
func pairCommands(cs []Change) []Change {
	argv := func(v any) []string {
		m, _ := v.(map[string]any)
		return strs(m["argv"])
	}
	var out []Change
	used := map[int]bool{}
	for i, rm := range cs {
		if rm.Op != "removed" {
			continue
		}
		a := argv(rm.Before)
		best, score := -1, 0.5
		for j, add := range cs {
			if add.Op != "added" || used[j] {
				continue
			}
			b := argv(add.After)
			if len(a) == 0 || len(b) == 0 || a[0] != b[0] {
				continue
			}
			s := overlap(a[1:], b[1:])
			if oneDiffers(a, b) {
				s = max(s, 0.5) // the same flags, one argument (a script) changed
			}
			if s >= score {
				best, score = j, s
			}
		}
		if best < 0 {
			continue
		}
		used[best], used[i] = true, true
		from, to := rm.Path[3], cs[best].Path[3]
		d, stats := unifiedDiff(strings.ReplaceAll(from, " -", "\n-"), strings.ReplaceAll(to, " -", "\n-"))
		out = append(out, Change{Op: "changed", Path: cs[best].Path, Before: from, After: to, TextDiff: &d, Stats: &stats})
	}
	for i, c := range cs {
		if !used[i] {
			out = append(out, c)
		}
	}
	return out
}

// overlap is how much of two argument lists is the same: the arguments both have over those
// either has. Two empty lists are the same.
func overlap(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	set := map[string]int{}
	for _, x := range a {
		set[x] |= 1
	}
	for _, x := range b {
		set[x] |= 2
	}
	both := 0
	for _, v := range set {
		if v == 3 {
			both++
		}
	}
	return float64(both) / float64(len(set))
}

// oneDiffers reports two argument lists of the same length that differ in one place.
func oneDiffers(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	n := 0
	for i := range a {
		if a[i] != b[i] {
			n++
		}
	}
	return n == 1
}

// shortCommand is a command line in a title: its first line, at most 100 characters.
func shortCommand(cmd string) string {
	line, _, multi := strings.Cut(cmd, "\n")
	if len(line) > 100 {
		line, multi = line[:100], true
	}
	if multi {
		line = strings.TrimSpace(line) + " …"
	}
	return line
}
