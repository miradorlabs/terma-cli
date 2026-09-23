package cmd

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// termaRun is how a test executes the command tree, and the only place one is built
// for execution. The zero value runs with no deadline and no extra environment.
type termaRun struct {
	// within bounds the run. A path that would wait at the browser, or on a feed,
	// then fails the test in seconds instead of hanging it for the login timeout.
	within time.Duration
	// env is set for the length of the test before the command runs.
	env map[string]string
}

// within is a run bounded by d. It is a call rather than a literal because a composite
// literal cannot open an if statement, which is where most runs are written.
func within(d time.Duration) termaRun { return termaRun{within: d} }

// exec runs `terma args...` and returns the two streams apart. Every run drops the
// update notice — a network call, and a line on stderr that belongs to no test — and
// starts from zeroed global flags, so one test's --project is never the next one's.
func (r termaRun) exec(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	for k, v := range r.env {
		t.Setenv(k, v)
	}
	flags = globalFlags{}

	ctx := context.Background()
	if r.within > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.within)
		defer cancel()
	}

	root := NewRootCommand()
	root.PersistentPostRun = nil
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err = root.ExecuteContext(ctx)
	return out.String(), errOut.String(), err
}

// combined is exec with the streams read as one, which is what an assertion about
// what the developer saw wants.
func (r termaRun) combined(t *testing.T, args ...string) (string, error) {
	t.Helper()
	stdout, stderr, err := r.exec(t, args...)
	return stdout + stderr, err
}

// runTerma is the everyday form: no deadline, no extra environment, one stream.
func runTerma(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return termaRun{}.combined(t, args...)
}

// fakeGateway serves handler as the API and the auth host, and returns the
// environment that points a run at it: authenticated with a server key, so no
// credential file is involved and no sign-in can be reached.
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
