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
	"strconv"
	"time"
)

// loginTimeout bounds the wait at the browser, so an abandoned attempt releases the port.
const loginTimeout = 5 * time.Minute

// TokenExchanger performs the code exchange against the API gateway.
type TokenExchanger interface {
	ExchangeCode(ctx context.Context, code, verifier string, port int) (*Credential, error)
}

// LoginOptions is what a browser sign-in needs beyond the token exchange.
type LoginOptions struct {
	// AppURL is the web app that serves the approval page, <AppURL>/cli/auth.
	AppURL string
	Label  string
	// Organization is only preselected; the user may pick another, so callers check.
	Organization string
	// NoBrowser prints the URL instead of opening it.
	NoBrowser bool
	Out       io.Writer
	// Timeout overrides loginTimeout when nonzero.
	Timeout time.Duration
}

// Login runs the browser handoff, binding the loopback port before the browser opens so
// no other process can race onto it.
func Login(ctx context.Context, exchanger TokenExchanger, opts LoginOptions) (*Credential, error) {
	out := opts.Out
	if out == nil {
		out = os.Stderr
	}

	pkce, err := NewPKCE()
	if err != nil {
		return nil, err
	}
	state, err := newState()
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen on loopback: %w", err)
	}
	defer func() { _ = listener.Close() }()

	port := listener.Addr().(*net.TCPAddr).Port

	type callback struct {
		code string
		err  error
	}
	results := make(chan callback, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		// A DNS-rebinding page reaches this handler with its own Host; reject it.
		if !isLoopbackRequest(r, port) {
			http.Error(w, "invalid host", http.StatusBadRequest)
			return
		}

		query := r.URL.Query()
		gotState := query.Get("state")
		code := query.Get("code")

		// Ignore a mismatched callback rather than abort: anything that guesses the port
		// could otherwise deny the login.
		if subtle.ConstantTimeCompare([]byte(gotState), []byte(state)) != 1 {
			writeResultPage(w, http.StatusBadRequest, "Authorization failed", "This request did not match the login your terminal started.")
			return
		}
		if code == "" {
			writeResultPage(w, http.StatusBadRequest, "Authorization failed", "No authorization code was returned.")
			return
		}
		writeResultPage(w, http.StatusOK, "You're signed in", "You can close this tab and return to your terminal.")
		results <- callback{code: code}
	})

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
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

	authURL := buildAuthorizeURL(opts.AppURL, pkce.Challenge, state, port, opts.Label, opts.Organization)
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
			return nil, res.err
		}
		return exchanger.ExchangeCode(ctx, res.code, pkce.Verifier, port)
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("timed out after %s waiting for browser authorization", timeout)
		}
		return nil, ctx.Err()
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

func buildAuthorizeURL(appURL, challenge, state string, port int, label, organization string) string {
	u := appURL + "/cli/auth"
	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("state", state)
	q.Set("port", strconv.Itoa(port))
	if label != "" {
		q.Set("label", label)
	}
	if organization != "" {
		q.Set("org", organization)
	}
	return u + "?" + q.Encode()
}

// DefaultLabel names the machine in the approval prompt and the session list.
func DefaultLabel() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return runtime.GOOS
	}
	return host
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
