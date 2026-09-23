package live

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	code := m.Run()
	if Enabled() {
		dir := os.Getenv("TERMA_LIVE_REPORT")
		if dir == "" {
			dir = "report"
		}
		if err := WriteReport(dir); err != nil {
			os.Stderr.WriteString("live report: " + err.Error() + "\n")
		}
	}
	os.Exit(code)
}

// track records the test's outcome for the report when it finishes.
func track(t *testing.T) {
	t.Cleanup(func() {
		switch {
		case t.Failed():
			Record(t.Name(), "fail", "")
		case t.Skipped():
			Record(t.Name(), "not run", "skipped; see log")
		default:
			Record(t.Name(), "pass", "")
		}
	})
}

// claudeMode picks how Claude Code logs in for this run: an OAuth token from
// `claude setup-token` runs the isolated shape; TERMA_LIVE_REAL_LOGIN=1 uses the
// developer's own login; neither means the subscription scenarios are not run.
func claudeMode(t *testing.T) (Mode, Route) {
	t.Helper()
	creds := ClaudeCredentials()
	switch {
	case creds.OAuthToken != "":
		return Isolated, RouteSubscription
	case os.Getenv("TERMA_LIVE_REAL_LOGIN") == "1":
		return RealLogin, RouteSubscription
	}
	Record(t.Name(), "not run", "needs CLAUDE_CODE_OAUTH_TOKEN (claude setup-token) or TERMA_LIVE_REAL_LOGIN=1")
	t.Skip("no subscription credential for Claude Code")
	return 0, ""
}
