package refdiff

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// A change's title: one line a person reads, by its category and what it changed, with its
// values where they are short.

// title is a change said in a line.
func title(c *Change, labels map[string]string) string {
	p := c.Path
	at := func(i int) string {
		if i < len(p) {
			return p[i]
		}
		return ""
	}
	verb := map[string]string{"added": "added", "removed": "removed", "changed": "changed"}[c.Op]
	short := func(v any) string {
		switch x := v.(type) {
		case nil:
			return "none"
		case string:
			if len(x) > 60 {
				return x[:57] + "…"
			}
			return x
		case float64:
			return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%f", x), "0"), ".")
		}
		data, _ := json.Marshal(v)
		if len(data) > 60 {
			return string(data[:57]) + "…"
		}
		return string(data)
	}
	arrow := func() string { return short(c.Before) + " → " + short(c.After) }
	stats := ""
	if c.Stats != nil {
		stats = fmt.Sprintf(" (+%d −%d chars)", c.Stats.AddedChars, c.Stats.RemovedChars)
	}
	switch c.Category {
	case "prompt":
		what := map[string]string{"system": "System prompt", "context": "Injected context", "interactive": "Interactive system prompt"}[at(1)]
		if what == "" {
			what = "Prompt " + at(1)
		}
		if at(1) == "interactive" && at(2) == "context" {
			what = "Interactive session's injected context"
		}
		if c.Op != "changed" && (at(1) == "system" || at(1) == "context") && len(p) == 3 {
			return fmt.Sprintf("%s block %s", what, verb)
		}
		return what + " " + verb + stats
	case "tools":
		switch {
		case len(p) == 2 && c.Op == "added" && deferredValue(c.After):
			return "New deferred tool: " + at(1)
		case len(p) == 2 && c.Op == "added":
			return "New tool: " + at(1)
		case len(p) == 2 && c.Op == "removed":
			return "Tool removed: " + at(1)
		case at(2) == "deferred" && c.Op == "added":
			return at(1) + " is now a deferred tool (named only; its schema loaded on demand)"
		case at(2) == "deferred" && c.Op == "removed":
			return at(1) + " is no longer deferred: offered with its schema"
		case at(2) == "description":
			return at(1) + " tool description " + verb + stats
		case at(2) == "input_schema" && at(3) == "properties" && len(p) == 5:
			return fmt.Sprintf("%s tool parameter %s %s", at(1), at(4), verb)
		case at(2) == "input_schema" && at(3) == "properties" && len(p) > 5:
			return fmt.Sprintf("%s tool parameter %s: %s %s%s", at(1), at(4), strings.Join(p[5:], "."), verb, stats)
		case at(2) == "input_schema":
			return at(1) + " tool parameters " + verb
		}
		return at(1) + " tool " + strings.Join(p[2:], ".") + " " + verb
	case "model":
		switch at(1) {
		case "default":
			return "Default model: " + arrow()
		case "max_tokens":
			return "Max output tokens: " + arrow()
		case "aliases":
			return fmt.Sprintf("Model alias %q: %s", at(2), arrow())
		case "prices":
			return "Built-in price for " + at(2) + " " + verb
		case "params":
			if len(p) == 3 && c.Op != "changed" {
				return "Model parameter " + at(2) + " " + verb
			}
			return "Model parameter " + strings.Join(p[2:], ".") + ": " + arrow()
		}
	case "caching":
		if at(1) == "breakpoints" {
			return "Cache breakpoint " + strings.Join(p[2:], ".") + " " + verb
		}
		return "Caching " + strings.Join(p[1:], ".") + ": " + arrow()
	case "request":
		switch at(1) {
		case "betas":
			if c.Op == "added" {
				return "New beta: " + at(2)
			}
			return "Beta dropped: " + at(2)
		case "headers":
			if c.Op == "changed" && c.TextDiff != nil {
				return "Header " + at(2) + " changed" + stats
			}
			if c.Op == "changed" {
				return "Header " + at(2) + ": " + arrow()
			}
			return "Header " + at(2) + " " + verb
		case "identifiers":
			if c.Op == "added" {
				return "Now sends identifier " + at(2)
			}
			return "No longer sends identifier " + at(2)
		case "body_keys":
			return "Request field " + at(2) + " " + verb
		}
		return "Request " + strings.Join(p[1:], ".") + " " + verb
	case "side_requests":
		if len(p) == 2 {
			return "Side request " + at(1) + " " + verb
		}
		return "Side request " + at(1) + " " + strings.Join(p[2:], ".") + " " + verb
	case "behavior":
		return "Behaviour: " + strings.Join(p[1:], " ") + " " + verb + valueNote(c)
	case "telemetry":
		if at(1) == "surfaces" {
			switch {
			case len(p) == 3 && c.Op == "added":
				return fmt.Sprintf("New telemetry surface: %s%s", at(2), keyCount(c.After))
			case len(p) == 3 && c.Op == "removed":
				return "Telemetry surface no longer sent: " + at(2)
			case at(3) == "keys" && len(p) == 5 && c.Op == "added":
				return fmt.Sprintf("New telemetry field: %s on %s", at(4), at(2))
			case at(3) == "keys" && len(p) == 5 && c.Op == "removed":
				return fmt.Sprintf("Telemetry field no longer sent: %s on %s", at(4), at(2))
			case at(3) == "keys" && len(p) > 5:
				return fmt.Sprintf("Telemetry field %s on %s: %s %s", at(4), at(2), strings.Join(p[5:], "."), arrow())
			}
		}
		return "Telemetry " + strings.Join(p[1:], ".") + " " + verb + valueNote(c)
	case "hooks":
		return hookTitle(c, verb, arrow)
	case "cli":
		if at(1) == "help" {
			return "Full help text of `" + at(2) + "` " + verb + stats
		}
		if at(1) == "launch" {
			return launchTitle(c, verb, arrow, short)
		}
		if at(1) == "app_server" && len(p) == 4 {
			if c.Op == "added" {
				return "New app-server method (" + at(2) + "): `" + short(c.After) + "`"
			}
			return "App-server method removed (" + at(2) + "): `" + short(c.Before) + "`"
		}
		if at(1) != "commands" {
			break
		}
		cmd := at(2)
		if len(p) == 3 {
			if c.Op == "added" {
				return "New command: `" + cmd + "`"
			}
			return "Command removed: `" + cmd + "`"
		}
		switch at(3) {
		case "options", "arguments":
			what, name := "flag", at(4)
			if at(3) == "arguments" {
				what = "argument"
			}
			subject := "`" + cmd + " " + name + "`"
			switch {
			case len(p) == 5 && c.Op == "added":
				return "New " + what + " " + subject
			case len(p) == 5:
				return strings.ToUpper(what[:1]) + what[1:] + " removed: " + subject
			}
			switch at(5) {
			case "description":
				if c.TextDiff != nil {
					return subject + ": description " + verb + stats
				}
				return subject + ": description " + verb
			case "default":
				return "Default of " + subject + ": " + arrow()
			case "choices":
				if c.Op == "added" {
					return subject + " gained value `" + at(6) + "`"
				}
				return subject + " lost value `" + at(6) + "`"
			case "names":
				if c.Op == "added" {
					return subject + " gained alias `" + at(6) + "`"
				}
				return subject + " lost alias `" + at(6) + "`"
			case "value":
				switch c.Op {
				case "added":
					return subject + " now takes a value " + short(c.After)
				case "removed":
					return subject + " no longer takes a value"
				}
				return subject + " now takes " + short(c.After) + " (was " + short(c.Before) + ")"
			case "env":
				return subject + " environment variable: " + arrow()
			}
		case "description":
			return "Description of `" + cmd + "` " + verb
		case "usage":
			return "Usage of `" + cmd + "` " + verb
		}
	case "config":
		if t := configTitle(c, arrow, stats); t != "" {
			return t
		}
	case "network":
		if at(1) == "telemetry" {
			switch {
			case len(p) == 2 && c.Op == "added":
				return "Now sends its own telemetry to its vendor"
			case len(p) == 2 && c.Op == "removed":
				return "No longer sends its own telemetry to its vendor"
			case len(p) == 3 && c.Op == "added":
				return "New vendor telemetry endpoint: " + at(2)
			case len(p) == 3:
				return "Vendor telemetry endpoint no longer used: " + at(2)
			case len(p) == 5 && c.Op == "added":
				return "New vendor telemetry event: " + at(4)
			case len(p) == 5:
				return "Vendor telemetry event no longer sent: " + at(4)
			case len(p) == 7 && at(5) == "fields":
				return fmt.Sprintf("%s telemetry event field %s %s", at(4), at(6), verb)
			}
		}
		if len(p) == 3 {
			if c.Op == "added" {
				return "New network request: " + at(2)
			}
			if c.Op == "removed" {
				return "Network request no longer made: " + at(2)
			}
		}
		if len(p) > 3 {
			return "Network request " + at(2) + " " + strings.Join(p[3:], ".") + " " + verb
		}
		return "Network " + strings.Join(p[1:], ".") + " " + verb
	case "storage":
		return "On-disk " + strings.Join(p[1:], " ") + " " + verb + valueNote(c)
	case "filesystem":
		return filesystemTitle(c)
	case "binary":
		if at(1) == "agent" && at(2) == "version" {
			return "Runs its agent's build " + short(c.After) + " (was " + short(c.Before) + ")"
		}
		if at(1) == "app" && at(2) == "signing" {
			switch at(3) {
			case "status":
				if c.After == "revoked" {
					return "The app's signing certificate revoked"
				}
				return "The app's code signature status: " + arrow()
			case "certificate":
				return "The app is signed with a different certificate"
			case "team_id":
				return "The app is signed by a different team: " + arrow()
			case "notarized":
				return "The app's notarization: " + arrow()
			}
			return "The app's code signature " + strings.Join(p[3:], ".") + ": " + arrow()
		}
		if at(1) == "signing" {
			switch at(2) {
			case "status":
				if c.After == "revoked" {
					return "Signing certificate revoked"
				}
				return "Code signature status: " + arrow()
			case "certificate":
				return "Signed with a different certificate"
			case "team_id":
				return "Signed by a different team: " + arrow()
			case "format":
				return "Code signature: " + arrow()
			}
			return "Code signature " + strings.Join(p[2:], ".") + ": " + arrow()
		}
		if at(1) == "size_bytes" {
			b, okB := c.Before.(float64)
			a, okA := c.After.(float64)
			if okB && okA {
				return fmt.Sprintf("Binary size: %s → %s bytes (%s → %s)", grouped(b), grouped(a), humanBytes(b), humanBytes(a))
			}
		}
		return "Binary " + strings.Join(p[1:], ".") + ": " + arrow()
	case "support":
		if len(p) >= 4 && at(3) == "result" {
			id, platform, _ := strings.Cut(at(2), "@")
			label := id
			if l := labels[at(2)]; l != "" {
				label = l
			}
			if m, ok := c.After.(string); ok && m == "fail" {
				return fmt.Sprintf("Terma: %s now fails on %s", label, platform)
			}
			if c.After == "pass" {
				return fmt.Sprintf("Terma: %s now passes on %s", label, platform)
			}
			return fmt.Sprintf("Terma: %s now %s on %s", label, short(c.After), platform)
		}
		if at(1) == "status" {
			return "Terma support: " + arrow()
		}
	}
	return strings.Join(p, ".") + " " + verb + valueNote(c)
}

// launchTitle is a change to how a desktop app starts its agent (cli.launch) said in a line.
func launchTitle(c *Change, verb string, arrow func() string, short func(any) string) string {
	p := c.Path
	switch {
	case len(p) == 4 && p[2] == "argv" && c.Op == "added":
		return "The app now starts its agent with `" + short(c.After) + "`"
	case len(p) == 4 && p[2] == "argv" && c.Op == "removed":
		return "The app no longer starts its agent with `" + short(c.Before) + "`"
	case len(p) == 4 && p[2] == "env" && c.Op == "added":
		return "The app now sets its agent's " + short(c.After)
	case len(p) == 4 && p[2] == "env" && c.Op == "removed":
		return "The app no longer sets its agent's " + short(c.Before)
	case len(p) == 4 && p[2] == "env_values":
		return "The app's agent's " + p[3] + ": " + arrow()
	case len(p) >= 3 && p[2] == "parents":
		return "The processes between the app and its agent " + verb
	}
	return "How the app starts its agent: " + strings.Join(p[2:], ".") + " " + verb
}

// hookTitle is a change to the hooks section said in a line: the events a build knows
// (supported), and what each that fired was handed and did with a hook's answer (events).
func hookTitle(c *Change, verb string, arrow func() string) string {
	p := c.Path
	at := func(i int) string {
		if i < len(p) {
			return p[i]
		}
		return ""
	}
	event := at(2)
	switch {
	case at(1) == "supported" && c.Op == "added":
		return "New hook event: " + event
	case at(1) == "supported" && c.Op == "removed":
		return "Hook event no longer supported: " + event
	case at(1) == "source":
		return "Hook events read from: " + arrow()
	case at(1) != "events":
	case len(p) == 3 && c.Op == "added":
		return "Hook event now fired: " + event
	case len(p) == 3 && c.Op == "removed":
		return "Hook event no longer fired: " + event
	case len(p) == 5 && at(3) == "keys":
		return fmt.Sprintf("%s hook payload field %s %s", event, at(4), verb)
	case len(p) == 5 && at(3) == "kinds":
		return fmt.Sprintf("%s hook payload field %s: %s", event, at(4), arrow())
	case len(p) == 5 && at(3) == "env":
		return fmt.Sprintf("%s hook environment variable %s %s", event, at(4), verb)
	case len(p) == 5 && at(3) == "env_paths":
		return fmt.Sprintf("%s hook environment variable %s: %s", event, at(4), arrow())
	case len(p) == 5 && at(3) == "control" && c.Op == "changed":
		return fmt.Sprintf("%s hook control (%s): %s", event, at(4), arrow())
	case len(p) == 5 && at(3) == "control":
		return fmt.Sprintf("%s hook control (%s) %s", event, at(4), verb)
	case len(p) == 4 && at(3) == "matcher":
		return fmt.Sprintf("%s hook takes a matcher: %s", event, arrow())
	case len(p) == 4 && at(3) == "cwd":
		return fmt.Sprintf("%s hook working directory: %s", event, arrow())
	case len(p) == 4 && c.Op == "changed":
		return fmt.Sprintf("%s hook %s: %s", event, at(3), arrow())
	}
	return "Hook " + strings.Join(p[1:], ".") + " " + verb
}

// valueNote is ": before → after" for a short change of value, nothing otherwise.
func valueNote(c *Change) string {
	if c.Op != "changed" || c.TextDiff != nil {
		return ""
	}
	b, _ := json.Marshal(c.Before)
	a, _ := json.Marshal(c.After)
	if len(a)+len(b) > 80 {
		return ""
	}
	return ": " + string(b) + " → " + string(a)
}

func keyCount(v any) string {
	m, _ := v.(map[string]any)
	keys, _ := m["keys"].(map[string]any)
	if len(keys) == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d fields)", len(keys))
}

// grouped is a whole number with thousands separators: 212,412,032.
func grouped(n float64) string {
	s := strconv.FormatFloat(n, 'f', 0, 64)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// humanBytes is a byte count in decimal units, as Finder shows it: 212.4 MB.
func humanBytes(n float64) string {
	units := []string{"bytes", "KB", "MB", "GB", "TB"}
	i := 0
	for n >= 1000 && i < len(units)-1 {
		n /= 1000
		i++
	}
	if i == 0 {
		return strconv.FormatFloat(n, 'f', 0, 64) + " bytes"
	}
	return strconv.FormatFloat(n, 'f', 1, 64) + " " + units[i]
}
