package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/machine"
)

// loginTimeout bounds the wait at the browser, so an abandoned attempt releases the port.
const loginTimeout = 5 * time.Minute

// maxCallbackBody bounds a posted callback, which carries only a state and its codes.
const maxCallbackBody = 8 << 10

// MaxCodes bounds the organizations one approval signs in to (contract C7): the approval
// page ticks at most 20, as many exchanges as the auth host's CLI bucket takes at once, so
// a link pre-checks at most 20 and a callback carrying more is refused.
const MaxCodes = 20

// LoginResult is one organization's completed sign-in, with the names to print.
type LoginResult struct {
	Credential       *Credential
	OrganizationName string
	UserID           string
}

// TokenExchanger performs the code exchange against the API gateway.
type TokenExchanger interface {
	// ExchangeCode redeems a browser approval's code, sent to the listener on port, with
	// the organization it signs in to: each code is its own organization's.
	ExchangeCode(ctx context.Context, code, verifier string, port int) (*LoginResult, error)
	// ExchangeDelegatedCode redeems a code Terma Desktop obtained for this process's
	// challenge, which no listener took (redirect_port 0).
	ExchangeDelegatedCode(ctx context.Context, code, verifier string) (*Credential, error)
}

// LoginOptions is what a browser sign-in needs beyond the token exchange.
type LoginOptions struct {
	// AppURL is the web app that serves the approval page, <AppURL>/cli/auth.
	AppURL string
	Label  string
	// Organization is only preselected; the user may pick another, so callers check.
	Organization string
	// Organizations are the ones the approval page pre-checks, at most MaxCodes; none
	// pre-checks every organization the user belongs to. The user may tick more.
	Organizations []string
	// NoBrowser prints the URL instead of opening it.
	NoBrowser bool
	Out       io.Writer
	// Timeout overrides loginTimeout when nonzero.
	Timeout time.Duration
}

// LoginAll runs the browser handoff, binding the loopback port before the browser opens so
// no other process can race onto it. The approval page sends one code per organization
// ticked there, and each is exchanged in turn with the one verifier. It returns every
// sign-in, in the order the page sent them, and every exchange that failed; one that
// fails leaves only its own organization without a sign-in. Only when none succeeds does
// it fail, with the first exchange's error.
func LoginAll(ctx context.Context, exchanger TokenExchanger, opts LoginOptions) (signedIn []LoginResult, failed []error, err error) {
	out := opts.Out
	if out == nil {
		out = os.Stderr
	}

	pkce, err := NewPKCE()
	if err != nil {
		return nil, nil, err
	}
	state, err := newState()
	if err != nil {
		return nil, nil, fmt.Errorf("generate state: %w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("listen on loopback: %w", err)
	}
	defer func() { _ = listener.Close() }()

	port := listener.Addr().(*net.TCPAddr).Port

	type callback struct {
		codes []string
		err   error
	}
	results := make(chan callback, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		// A DNS-rebinding page reaches this handler with its own Host; reject it.
		if !isLoopbackRequest(r, port) {
			http.Error(w, "invalid host", http.StatusBadRequest)
			return
		}

		gotState, codes, ok := callbackParams(w, r)
		if !ok {
			return
		}

		// Ignore a mismatched callback rather than abort: anything that guesses the port
		// could otherwise deny the login.
		if subtle.ConstantTimeCompare([]byte(gotState), []byte(state)) != 1 {
			writeResultPage(w, http.StatusBadRequest, "Authorization failed", "This request did not match the login your terminal started.")
			return
		}
		if len(codes) == 0 || slices.Contains(codes, "") {
			writeResultPage(w, http.StatusBadRequest, "Authorization failed", "No authorization code was returned.")
			return
		}
		writeResultPage(w, http.StatusOK, "You're signed in", "You can close this tab and return to your terminal.")
		results <- callback{codes: codes}
	})

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			results <- callback{err: fmt.Errorf("loopback server: %w", err)}
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	authURL := buildAuthorizeURL(opts.AppURL, pkce.Challenge, state, port, opts.Label, opts.Organization, opts.Organizations)
	if opts.NoBrowser {
		fmt.Fprintf(out, "Open this URL to authorize the CLI:\n\n  %s\n\n", authURL)
	} else {
		fmt.Fprintf(out, "Opening your browser to authorize the CLI.\nIf it does not open, visit:\n\n  %s\n\n", authURL)
		if err := openBrowser(authURL); err != nil {
			fmt.Fprintf(out, "Could not open a browser automatically (%v). Use the URL above.\n\n", err)
		}
	}
	fmt.Fprintln(out, "Waiting for authorization...")

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = loginTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case res := <-results:
		if res.err != nil {
			return nil, nil, res.err
		}
		return exchangeAll(ctx, exchanger, res.codes, pkce.Verifier, port)
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, nil, fmt.Errorf("timed out after %s waiting for browser authorization", timeout)
		}
		return nil, nil, ctx.Err()
	}
}

// exchangeAll redeems codes one after another with the one verifier, keeping each that
// succeeds; it fails, with the first error, only when every exchange does.
func exchangeAll(ctx context.Context, exchanger TokenExchanger, codes []string, verifier string, port int) ([]LoginResult, []error, error) {
	var signedIn []LoginResult
	var failed []error
	for _, code := range codes {
		res, err := exchanger.ExchangeCode(ctx, code, verifier, port)
		switch {
		case err != nil:
			failed = append(failed, err)
		case res == nil || res.Credential == nil:
			failed = append(failed, errors.New("token endpoint returned no credential"))
		default:
			signedIn = append(signedIn, *res)
		}
	}
	if len(signedIn) == 0 {
		return nil, failed, failed[0]
	}
	return signedIn, failed, nil
}

// callbackParams reads the state and the codes the approval page sends back: from the
// form body of a POST (application/x-www-form-urlencoded, as a plain HTML form sends it),
// one repeated code field per organization ticked, in the page's order, which keeps the
// codes out of the address bar and the browser's history; or the one code in the query of
// a GET, as the page has sent it so far. Either way the caller checks the state, so a page
// that posts here without it is ignored, as a GET is. More than MaxCodes is malformed. ok
// is false once a response has been written.
func callbackParams(w http.ResponseWriter, r *http.Request) (state string, codes []string, ok bool) {
	switch r.Method {
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, maxCallbackBody)
		if err := r.ParseForm(); err != nil || len(r.PostForm["code"]) > MaxCodes {
			writeResultPage(w, http.StatusBadRequest, "Authorization failed", "The authorization response could not be read.")
			return "", nil, false
		}
		return r.PostForm.Get("state"), r.PostForm["code"], true
	case http.MethodGet:
		query := r.URL.Query()
		if code := query.Get("code"); code != "" {
			codes = []string{code}
		}
		return query.Get("state"), codes, true
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return "", nil, false
	}
}

// isLoopbackRequest reports whether the request is addressed to this listener on loopback.
func isLoopbackRequest(r *http.Request, port int) bool {
	host, hostPort, err := net.SplitHostPort(r.Host)
	if err != nil {
		return false
	}
	if hostPort != strconv.Itoa(port) {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// buildAuthorizeURL is the approval page's link. response_mode=form_post (the name OAuth gives
// the same handoff) tells the page this listener takes the code as a form POST, so the page
// can tell a CLI that reads only a GET to update rather than send it a code it would ignore.
// codes=multiple tells it this listener takes one code per organization ticked (contract
// C7), so the page shows every organization to tick; an older CLI, which never sends it,
// gets the one organization it always did. orgs are the organizations pre-checked, at
// most MaxCodes; without it the page pre-checks every one.
func buildAuthorizeURL(appURL, challenge, state string, port int, label, organization string, organizations []string) string {
	u := appURL + "/cli/auth"
	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("state", state)
	q.Set("port", strconv.Itoa(port))
	q.Set("response_mode", "form_post")
	q.Set("codes", "multiple")
	if label != "" {
		q.Set("label", label)
	}
	if organization != "" {
		q.Set("org", organization)
	}
	if len(organizations) > 0 {
		q.Set("orgs", strings.Join(organizations[:min(len(organizations), MaxCodes)], ","))
	}
	return u + "?" + q.Encode()
}

// DefaultLabel names the machine in the approval prompt and the session list.
func DefaultLabel() string {
	if host := machine.Name(); host != "" {
		return host
	}
	return runtime.GOOS
}

func openBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}

func writeResultPage(w http.ResponseWriter, status int, heading, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html>
<meta charset="utf-8">
<title>Terma CLI</title>
<style>
  body { font-family: ui-sans-serif, system-ui, sans-serif; display: grid; place-items: center; min-height: 100vh; margin: 0; background: #0b0d10; color: #e6e8eb; }
  main { text-align: center; max-width: 26rem; padding: 2rem; }
  h1 { font-size: 1.25rem; margin: 0 0 .5rem; }
  p { margin: 0; color: #9aa3ad; line-height: 1.5; }
</style>
<main><h1>%s</h1><p>%s</p></main>
`, heading, detail)
}
