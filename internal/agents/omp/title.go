package omp

import (
	"context"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// An interactive OMP names its session after the first reply, from the first prompt, and
// keeps the name in its transcript, never in its telemetry. terma's extension reads it at
// each turn's start and end and at shutdown, and hands a new or changed one to omp-title,
// which spools it as terma.session.title under titleConsented and, at delivery, the policy's
// prompt switch. A headless `omp -p` names nothing.

// sourceOmpSession is where a session's name comes from: OMP's session transcript.
const sourceOmpSession = "omp_session"

type titleHookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Title     string `json:"title"`
}

var ompTitleStateDir = hookrun.AgentStateDir(name, "titles")

// title is the omp-title hook: it spools the session's name when it differs from the one
// last spooled. Anything it cannot use is ignored: a hook that fails would surface in OMP.
func title(ctx context.Context, env hookrun.Env) error {
	in, err := hookrun.ReadInput[titleHookInput](env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	sessionName := strings.TrimSpace(in.Title)
	if sessionName == "" {
		return nil
	}
	r, ok := env.Open(ctx, in.SessionID, in.Cwd)
	if !ok || !titleConsented(env.Consent(r)) {
		return nil
	}
	attrs := map[string]any{semconv.GenAIMainAgentNameKey: name, semconv.TermaEvidenceSourceKey: sourceOmpSession}
	hookrun.SpoolTitle(env, r, ompTitleStateDir, in.SessionID, attrs, func() (string, bool, error) {
		return sessionName, true, nil
	})
	return nil
}

// titleConsented reports whether a title may be sent at all: OMP is among the developer's
// agents, which setup points at the relay, or global mode collects every session. The team's
// policy, checked at delivery, decides whether prompts may.
func titleConsented(c hookrun.Consent) bool {
	return c.Global || slices.Contains(c.Agents, name)
}
