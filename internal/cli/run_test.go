package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// termaRun is the one way a test executes the command tree; the zero value has no
// deadline and no extra environment.
type termaRun struct {
	// within bounds the run, so a wait at the browser or on a feed fails in seconds.
	within time.Duration
	env    map[string]string
}

// within is a run bounded by d, a call because a composite literal cannot open an if.
func within(d time.Duration) termaRun { return termaRun{within: d} }

// exec runs `terma args...` without the update notice and returns the two streams apart.
func (r termaRun) exec(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	for k, v := range r.env {
		t.Setenv(k, v)
	}
	// A run's flags are its own; the next test starts from zero.
	defer func() { testApp.flags = globalFlags{} }()

	ctx := context.Background()
	if r.within > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.within)
		defer cancel()
	}

	root := testApp.NewRootCommand()
	root.PersistentPostRun = nil
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err = root.ExecuteContext(ctx)
	return out.String(), errOut.String(), err
}

// combined is exec with the streams read as one.
func (r termaRun) combined(t *testing.T, args ...string) (string, error) {
	t.Helper()
	stdout, stderr, err := r.exec(t, args...)
	return stdout + stderr, err
}

func runTerma(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return termaRun{}.combined(t, args...)
}

// fakeGateway serves handler as the API and auth host and returns the environment for it,
// with a server key so no sign-in can be reached.
func fakeGateway(t *testing.T, handler http.HandlerFunc) map[string]string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return map[string]string{
		"TERMA_CONFIG_DIR": t.TempDir(),
		"TERMA_API_URL":    srv.URL,
		"TERMA_AUTH_URL":   srv.URL,
		"TERMA_API_KEY":    "ter_srv_0123456789abcdef",
		"TERMA_ENV":        "",
		"TERMA_PROFILE":    "",
	}
}
