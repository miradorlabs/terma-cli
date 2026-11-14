package runs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Only a file event, a commit or a push keeps its destination once its run turns Mixed:
// every record that carries content obeys the run, wherever it routes.
func TestOnlyPathsAndGitFactsAreExemptFromMixed(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		semconv.TermaFilesTouchedEvent:       true,
		semconv.TermaCommitEvent:             true,
		semconv.TermaCommitStampedEvent:      true,
		semconv.TermaCommitUnattributedEvent: true,
		semconv.TermaPushEvent:               true,
		semconv.TermaAssistantMessageEvent:   false,
		semconv.TermaSessionTitleEvent:       false,
		semconv.TermaToolCallEvent:           false,
		semconv.TermaSessionQuotaEvent:       false,
		semconv.TermaSessionStartEvent:       false,
		"":                                   false,
	} {
		if got := Locational(name); got != want {
			t.Errorf("Locational(%q) = %v, want %v", name, got, want)
		}
	}
}

// Locational events say where something happened and carry nothing from the conversation:
// no prompt, reply, tool input or output, file contents, commit message or diff. Each
// attribute the registry gives any of them is listed here with why it carries none, so an
// attribute added to one fails this test until it is judged. A field that can hold text the
// developer or agent wrote about their work, a commit message say, must never be listed:
// it would leave on a Mixed run. Strip it from the event, or take the event off
// Locational.
var locationalAttributes = map[string]string{
	// Identifiers terma or the agent mints, and counts.
	"session.id":                        "an id",
	"terma.agent.id":                    "an id",
	"gen_ai.tool.call.id":               "an id",
	"terma.turn.id":                     "an id",
	"terma.step.index":                  "a count",
	"terma.checkout.id":                 "a hash of the checkout's root",
	"terma.commit.session.ids":          "ids",
	"terma.commit.sessions":             "ids, each with its agent's name and version",
	"terma.commit.session.count":        "a count",
	"terma.commit.staged.count":         "a count",
	"terma.commit.file.count":           "a count",
	"terma.commit.lines.added":          "a count",
	"terma.commit.lines.deleted":        "a count",
	"terma.commit.file.stats":           "each file's path and counts, and the session id and agent that touched it",
	"terma.commit.file.stats_reported":  "a count",
	"terma.commit.file.stats_truncated": "a flag",
	"vcs.ref.head.revision":             "a commit id",
	"terma.push.id":                     "an id",
	"terma.push.commits":                "commit ids",
	"terma.push.commit.count":           "a count",
	"terma.push.commits_truncated":      "a flag",
	"terma.push.session.ids":            "ids",
	"terma.push.sessions":               "ids, each with its agent's name and version",
	"terma.push.new.revision":           "a commit id",
	"terma.push.old.revision":           "a commit id",
	"terma.push.forced":                 "a flag",
	"terma.push.status":                 "an enum",
	"terma.push.range":                  "an enum",
	"terma.commit.message_source":       "an enum: where git took the message from, never the message",
	"terma.capture.surface":             "an enum",
	"gen_ai.main_agent.name":            "the agent's name",
	"gen_ai.agent.name":                 "a subagent's configured name",
	"gen_ai.tool.name":                  "the tool's name, never its input",
	// The location itself. A path or a branch may be text the developer chose, but it names
	// where the work happened, which is what a Locational event is for.
	"terma.files.paths":       "paths in the checkout",
	"terma.repository.root":   "the checkout's path",
	"vcs.repository.url.full": "the repository",
	"vcs.repository.name":     "the repository",
	"vcs.owner.name":          "the repository's owner",
	"vcs.provider.name":       "the repository's host",
	"vcs.ref.head.name":       "the branch",
	"vcs.ref.head.type":       "an enum",
	"terma.push.local.ref":    "the branch",
	"terma.push.remote.ref":   "the branch",
	"terma.push.remote.name":  "the remote",
	"terma.push.remote.url":   "the remote",
}

func TestLocationalEventsCarryNoConversation(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join("..", "..", "semconv", "registry", "events.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Events []struct {
			Name       string `yaml:"name"`
			Attributes []struct {
				Ref string `yaml:"ref"`
			} `yaml:"attributes"`
		} `yaml:"events"`
	}
	if err := yaml.Unmarshal(b, &registry); err != nil {
		t.Fatal(err)
	}
	var exempt []string
	for _, ev := range registry.Events {
		if !Locational(ev.Name) {
			continue
		}
		exempt = append(exempt, ev.Name)
		for _, a := range ev.Attributes {
			if _, ok := locationalAttributes[a.Ref]; !ok {
				t.Errorf("%s carries %s, which may hold the conversation: judge it in locationalAttributes, strip it, or take %s off Locational", ev.Name, a.Ref, ev.Name)
			}
		}
	}
	// Belt and braces: no judged attribute is named for what the conversation holds.
	for key := range locationalAttributes {
		for _, content := range []string{"prompt", "content", "input", "output", "title", "diff", "body", "message"} {
			if strings.Contains(key, content) && key != "terma.commit.message_source" {
				t.Errorf("%s, named for %s, is judged free of the conversation", key, content)
			}
		}
	}
	if len(exempt) != 5 {
		t.Errorf("the registry has %d of the five Locational events: %v", len(exempt), exempt)
	}
}
