package delivery

import (
	"maps"
	"net/url"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// hookContent is where each hook event kind carries a tool's input: the one list delivery
// withholds by. Hooks spool events whole and know no policy; the team's policy when an
// event leaves decides what of it does, as the relay decides for exports. A reply or a
// thread's name is what was said through and through, so Allowed sends it or not, whole.
var hookContent = map[string][]string{
	semconv.TermaToolCallEvent:          {semconv.GenAIToolCallArgumentsKey},
	semconv.TermaApprovalRequestedEvent: {semconv.TermaApprovalReasonKey},
}

// contentFree are the event kinds that carry no content, or (a reply, a thread's name) that
// Allowed sends or withholds whole; terma.repository.root, which some of them name, Outgoing
// withholds with tool content. A kind in none of the three lists is withheld whenever the
// policy withholds any content, so a new kind leaks nothing until it is classified.
var contentFree = map[string]bool{
	semconv.TermaSessionStartEvent: true, semconv.TermaSessionEndEvent: true,
	semconv.TermaCompactionEvent: true, semconv.TermaFilesTouchedEvent: true,
	semconv.TermaCommitStampedEvent: true, semconv.TermaCommitEvent: true, semconv.TermaCommitUnattributedEvent: true,
	semconv.TermaPushEvent: true, semconv.TermaSessionQuotaEvent: true, semconv.TermaSessionAccountEvent: true, semconv.TermaSessionLimitEvent: true,
	semconv.TermaSessionCaptureEvent: true, semconv.TermaSubagentStartEvent: true, semconv.TermaSubagentEndEvent: true,
	semconv.TermaSubagentCallEvent: true, semconv.TermaSessionObservationEvent: true,
	semconv.TermaAssistantMessageEvent: true, semconv.TermaSessionTitleEvent: true,
	semconv.TermaModelCallResponseEvent: true,
}

// contentWhole are the event kinds that are content through and through, of both kinds, so
// neither switch alone covers them (a skill's definition: its description is what was said,
// its name and file are a tool's): sent whole while the policy collects prompts and tool
// content, dropped otherwise. Hooks check the policy before spooling one too; this catches
// one queued before the policy changed. A tool call that names the skill it loaded
// (terma.skill.name) loses that name by the same rule, and the call still leaves.
var contentWhole = map[string]bool{semconv.TermaSkillDefinitionEvent: true}

// Outgoing is e as it may leave for projectID under org, the team's policy now, without
// the content that policy withholds (config.Policy.Content, the relay's rule); false when
// none of it may leave. A policy withholding either kind of content withholds user.email
// too, as the relay does; an agent that keys a seat on it sends terma.account.seat.id beside it.
func (r Router) Outgoing(org config.Policy, projectID string, e spool.Event) (spool.Event, bool) {
	if !r.Allowed(org, projectID, e) {
		return spool.Event{}, false
	}
	pol := routing.EffectivePolicy(r.StateDir, org, projectID)
	prompts, toolContent := pol.Content()
	if prompts && toolContent {
		return skillLinkAdmitted(pol, e), true
	}
	toolKeys, ok := hookContent[e.Name]
	if contentWhole[e.Name] || !ok && !contentFree[e.Name] {
		return spool.Event{}, false
	}
	e.Attrs = maps.Clone(e.Attrs)
	delete(e.Attrs, semconv.UserEmailKey)
	delete(e.Attrs, semconv.TermaSkillNameKey)
	if !toolContent {
		for _, k := range toolKeys {
			delete(e.Attrs, k)
		}
		// A local path, on whichever event names its checkout: withheld as the relay withholds it.
		delete(e.Attrs, semconv.TermaRepositoryRootKey)
	}
	return e, true
}

// skillLinkAdmitted is a skill's definition without the place of its file once the policy
// no longer collects the repository that place names: a personal skill kept in another
// checkout names that checkout, which Allowed, judging the session's own, never sees. A
// plugin's skill names its marketplace, which no policy lists.
func skillLinkAdmitted(pol config.Policy, e spool.Event) spool.Event {
	link, _ := e.Attrs[semconv.TermaSkillRepositoryURLKey].(string)
	if e.Name != semconv.TermaSkillDefinitionEvent || link == "" || e.Attrs[semconv.TermaSkillOriginKey] == semconv.TermaSkillOriginPlugin {
		return e
	}
	if u, err := url.Parse(link); err == nil && pol.Admits(config.Repository{Origin: u.Host + u.Path}) {
		return e
	}
	e.Attrs = maps.Clone(e.Attrs)
	delete(e.Attrs, semconv.TermaSkillRepositoryURLKey)
	delete(e.Attrs, semconv.TermaSkillPathKey)
	return e
}

// wire is a batch as it is sent: the project leaves as the resource's mirador.project.id,
// so the routing key each event is queued under stays behind. Each event names the
// repository it was admitted under as vcs.repository.url.full, unless it names its own (a
// touch, commit or push), so the server keys every record as the policy admitted it, those
// queued before terma stamped it too.
func wire(batch []spool.Event) []spool.Event {
	out := make([]spool.Event, len(batch))
	for i, e := range batch {
		e.Attrs = maps.Clone(e.Attrs)
		delete(e.Attrs, hookrun.AttrProjectID)
		if _, named := e.Attrs[semconv.VCSRepositoryURLFullKey]; !named {
			if u := repositoryURL(e.Repository); u != "" {
				if e.Attrs == nil {
					e.Attrs = map[string]any{}
				}
				e.Attrs[semconv.VCSRepositoryURLFullKey] = u
			}
		}
		out[i] = e
	}
	return out
}

// repositoryURL is the https URL of the repository admitted as r, `host/path`, that the
// server keys back to r exactly: it drops one `.git`, as gitx.NormalizeRemote did before
// RepositoryID dropped another, so a key still ending in `.git` gets one back. "" outside git.
func repositoryURL(r config.Repository) string {
	host, path, ok := strings.Cut(r.Origin, "/")
	if !ok || host == "" || path == "" {
		return ""
	}
	if strings.HasSuffix(path, ".git") {
		path += ".git"
	}
	if strings.Contains(host, ":") { // an IPv6 literal; the key has no port
		host = "[" + host + "]"
	}
	return (&url.URL{Scheme: "https", Host: host, Path: "/" + path}).String()
}
