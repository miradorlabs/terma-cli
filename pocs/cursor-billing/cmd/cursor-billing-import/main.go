// Local validation runner only. The platform should import cursorbilling itself
// and inject credentials from its own secret store; no terma command is installed.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	billing "github.com/miradorlabs/terma-cli/pocs/cursor-billing"
)

type capture struct {
	Version      int                 `json:"version"`
	StartedAt    time.Time           `json:"started_at"`
	EndedAt      time.Time           `json:"ended_at"`
	TestPassed   bool                `json:"test_passed"`
	Observations []map[string]string `json:"observations"`
}

func readSessions(path string) ([]billing.Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 8<<20+1))
	if err != nil || len(b) > 8<<20 {
		return nil, fmt.Errorf("invalid or oversized capture")
	}
	var c capture
	if json.Unmarshal(b, &c) != nil || c.Version != 1 || !c.TestPassed {
		return nil, fmt.Errorf("capture must be a successful version 1 live test")
	}
	byID := map[string]*billing.Session{}
	turns := map[string]map[string]bool{}
	for _, o := range c.Observations {
		id := o["session.id"]
		if id == "" {
			continue
		}
		s := byID[id]
		if s == nil {
			s = &billing.Session{ConversationID: id}
			byID[id] = s
			turns[id] = map[string]bool{}
		}
		if email := o["account_email"]; email != "" {
			if s.UserEmail != "" && s.UserEmail != email {
				return nil, fmt.Errorf("account changed within captured session")
			}
			s.UserEmail = email
		}
		if o["hook_event"] == "stop" && o["turn_id"] != "" {
			turns[id][o["turn_id"]] = true
		}
	}
	result := []billing.Session{}
	for id, s := range byID {
		for turn := range turns[id] {
			s.TurnIDs = append(s.TurnIDs, turn)
		}
		sort.Strings(s.TurnIDs)
		result = append(result, *s)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ConversationID < result[j].ConversationID })
	if len(result) == 0 {
		return nil, fmt.Errorf("capture has no conversations")
	}
	return result, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cursor-billing-import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tenant := fs.String("tenant", "", "platform tenant ID for binding")
	connection := fs.String("connection", "", "platform Cursor connection ID for binding")
	startFlag := fs.String("start", "", "inclusive RFC3339 start; defaults to current billing cycle start")
	endFlag := fs.String("end", "", "exclusive RFC3339 end; defaults to now")
	output := fs.String("out", "", "new private JSON output file (must not exist)")
	capturePath := fs.String("capture", "", "optional live Cursor session.json to join")
	pageSize := fs.Int("page-size", 1000, "rows per page (1..1000)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	fail := func(err error) int { fmt.Fprintln(stderr, err); return 1 }
	if *output == "" || fs.NArg() != 0 {
		return fail(fmt.Errorf("-out is required; output contains private team data"))
	}
	scope := billing.Scope{TenantID: *tenant, ConnectionID: *connection}
	client, err := billing.New(billing.Config{APIKey: os.Getenv("CURSOR_ADMIN_API_KEY"), Scope: scope, PageSize: *pageSize})
	if err != nil {
		return fail(err)
	}
	var sessions []billing.Session
	if *capturePath != "" {
		sessions, err = readSessions(*capturePath)
		if err != nil {
			return fail(err)
		}
	}
	end := time.Now().UTC().Truncate(time.Millisecond)
	if *endFlag != "" {
		end, err = time.Parse(time.RFC3339Nano, *endFlag)
		if err != nil {
			return fail(fmt.Errorf("invalid -end timestamp"))
		}
	}
	var start time.Time
	if *startFlag != "" {
		start, err = time.Parse(time.RFC3339Nano, *startFlag)
		if err != nil {
			return fail(fmt.Errorf("invalid -start timestamp"))
		}
	}
	// Never overwrite an existing snapshot. Remove this file on any failure.
	f, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail(err)
	}
	completed := false
	defer func() {
		f.Close()
		if !completed {
			os.Remove(*output)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	members, err := client.Members(ctx)
	if err != nil {
		return fail(err)
	}
	spend, err := client.Spend(ctx)
	if err != nil {
		return fail(err)
	}
	if start.IsZero() {
		start = time.UnixMilli(int64(spend.CycleStart)).UTC()
	}
	usage, err := client.Usage(ctx, billing.Window{Start: start, End: end})
	if err != nil {
		return fail(err)
	}
	var joined *billing.JoinResult
	if sessions != nil {
		j, err := billing.JoinSessions(usage, scope, sessions)
		if err != nil {
			return fail(err)
		}
		joined = &j
	}
	report := struct {
		Version int                     `json:"version"`
		Members billing.MembersSnapshot `json:"members"`
		Spend   billing.SpendSnapshot   `json:"spend"`
		Usage   billing.UsageSnapshot   `json:"usage"`
		Joined  *billing.JoinResult     `json:"joined,omitempty"`
	}{1, members, spend, usage, joined}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	completed = true
	fmt.Fprintf(stdout, "Imported %d members, %d spend rows (%d pages), %d usage events (%d pages). Private output: %s\n", len(members.Members), len(spend.Members), spend.Pages, len(usage.Events), usage.Pages, *output)
	if joined != nil {
		matched := 0
		for _, s := range joined.Sessions {
			matched += s.MatchedEvents
		}
		fmt.Fprintf(stdout, "Joined %d usage events to %d captured conversations.\n", matched, len(joined.Sessions))
	}
	return 0
}
func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
