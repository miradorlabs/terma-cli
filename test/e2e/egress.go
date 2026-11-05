package e2e

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The recording proxy: everything a harness build contacts besides its model provider. An
// egress scenario points the build's HTTP(S) proxy at it and has the build trust a CA made for
// the run; a CONNECT is answered with a certificate for its host, signed on the fly, and each
// request inside is recorded (RecordEgress) and answered here, 200 and `{}`. Nothing is ever
// forwarded: the build's telemetry, feature flags and update checks reach no one. A host whose
// TLS the build refuses (its certificate is pinned) is recorded with the error, so it is known
// to exist. The fixture provider and the OTLP receiver are on loopback, which NO_PROXY keeps off
// the proxy.

// EgressProxy is a recording, locally answering HTTP and HTTPS proxy.
type EgressProxy struct {
	// URL is the proxy's address, for HTTPS_PROXY; CAFile the run's CA, PEM, for the build's
	// trust store.
	URL    string
	CAFile string

	srv   *http.Server
	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey

	mu       sync.Mutex
	leaves   map[string]*tls.Certificate
	requests []ProviderRequest
	// files are what the proxy answers a GET of host+path with, in place of `{}` (Serve).
	files map[string][]byte
	// calls are the model calls' windows, which place each request before, during or after them.
	calls [][2]time.Time
}

// StartEgressProxy starts a recording proxy, with a CA of its own, for t's lifetime.
func StartEgressProxy(t *testing.T) *EgressProxy {
	t.Helper()
	ca, key, err := newEgressCA()
	if err != nil {
		t.Fatal(err)
	}
	p := &EgressProxy{ca: ca, caKey: key, leaves: map[string]*tls.Certificate{}}
	p.CAFile = filepath.Join(t.TempDir(), "egress-ca.pem")
	if err := os.WriteFile(p.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.URL = "http://" + ln.Addr().String()
	p.srv = &http.Server{Handler: http.HandlerFunc(p.serveProxy), ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
	t.Cleanup(func() { _ = p.srv.Close() })
	return p
}

// Env is what points a build at the proxy and has it trust the run's CA, for every runtime a
// harness is built on: Node and Bun read NODE_EXTRA_CA_CERTS (and Node reads the proxy
// variables only with NODE_USE_ENV_PROXY), Codex's HTTP client CODEX_CA_CERTIFICATE or
// SSL_CERT_FILE (codex-rs http-client/src/custom_ca.rs), Python's requests REQUESTS_CA_BUNDLE.
// A client that took SSL_CERT_FILE for its whole trust store and went around the proxy would
// fail its TLS, which is as well: nothing here reaches the internet on purpose.
func (p *EgressProxy) Env() []string {
	var env []string
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		env = append(env, k+"="+p.URL)
	}
	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		env = append(env, k+"=127.0.0.1,localhost,::1")
	}
	for _, k := range []string{"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "CODEX_CA_CERTIFICATE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "GIT_SSL_CAINFO"} {
		env = append(env, k+"="+p.CAFile)
	}
	return append(env, "NODE_USE_ENV_PROXY=1")
}

// Provider wraps the fixture provider so the requests isModelCall picks (every POST when it is
// nil) mark the run's model calls, which egress is placed against.
func (p *EgressProxy) Provider(isModelCall func(*http.Request) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := r.Method == http.MethodPost
		if isModelCall != nil {
			model = isModelCall(r)
		}
		start := time.Now().UTC()
		next.ServeHTTP(w, r)
		if model {
			p.mu.Lock()
			p.calls = append(p.calls, [2]time.Time{start, time.Now().UTC()})
			p.mu.Unlock()
		}
	})
}

// Requests are what the proxy has recorded so far, each placed against the model calls.
func (p *EgressProxy) Requests() []ProviderRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]ProviderRequest(nil), p.requests...)
	if len(p.calls) == 0 {
		return out
	}
	first, last := p.calls[0][0], p.calls[0][1]
	for _, c := range p.calls {
		if c[0].Before(first) {
			first = c[0]
		}
		if c[1].After(last) {
			last = c[1]
		}
	}
	for i := range out {
		switch at := out[i].At; {
		case at.Before(first):
			out[i].When = "before"
		case at.After(last):
			out[i].When = "after"
		default:
			out[i].When = "during"
		}
	}
	return out
}

// Record records every request the proxy saw in t's profile of b, in the order they arrived.
func (p *EgressProxy) Record(t *testing.T, b Binary) {
	t.Helper()
	reqs := p.Requests()
	sort.SliceStable(reqs, func(i, j int) bool { return reqs[i].At.Before(reqs[j].At) })
	for _, r := range reqs {
		RecordEgress(t, b, r)
	}
	Placeholder(t, b, p.URL, "<proxy>")
	Placeholder(t, b, p.CAFile, "<proxy-ca>")
}

func (p *EgressProxy) record(r ProviderRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, r)
}

// serveProxy takes a proxy request: a CONNECT is tunnelled to the proxy's own HTTPS (or HTTP)
// server for its host; a plain one names its URL whole, and is answered at once.
func (p *EgressProxy) serveProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.tunnel(w, r)
		return
	}
	host := r.URL.Host
	if host == "" {
		host = r.Host
	}
	p.answer(w, r, hostOf(host, "80"))
}

// Serve has the proxy answer a GET of host and path with body, a file the scenario fetched
// itself from where the build would: a desktop app that downloads its agent on first use (Claude
// Desktop) gets the same bytes it would have, and nothing is forwarded.
func (p *EgressProxy) Serve(host, path string, body []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.files == nil {
		p.files = map[string][]byte{}
	}
	p.files[host+path] = body
}

// answer records r, sent to host, and answers it the same whatever it asked, but for a file
// the scenario has it serve.
func (p *EgressProxy) answer(w http.ResponseWriter, r *http.Request, host string) {
	data, _ := io.ReadAll(io.LimitReader(r.Body, maxProfileBody))
	req := ProviderRequest{Host: host, Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		Header: RedactHeader(r.Header), Status: http.StatusOK, At: time.Now().UTC()}
	req.Body, req.Text = profileBody(r.Header.Get("Content-Encoding"), data)
	p.mu.Lock()
	file, served := p.files[host+r.URL.Path]
	p.mu.Unlock()
	if served && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		status := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		http.ServeContent(status, r, path.Base(r.URL.Path), time.Time{}, bytes.NewReader(file))
		req.Status = status.status
		p.record(req)
		return
	}
	p.record(req)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, "{}")
	}
}

// tunnel answers a CONNECT, then serves what comes through it: TLS with a certificate for
// the target, or plain HTTP (a ws:// through the proxy).
func (p *EgressProxy) tunnel(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijacking", http.StatusInternalServerError)
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = conn.Close()
		return
	}
	pc := &peekedConn{Conn: conn, r: buf.Reader}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	first, err := pc.r.Peek(1)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		return
	}
	at := time.Now().UTC()
	var served net.Conn = pc
	host := hostOf(target, "443")
	if first[0] == 0x16 { // a TLS handshake record
		name, _, _ := strings.Cut(target, ":")
		if h, _, err := net.SplitHostPort(target); err == nil {
			name = h
		}
		tc := tls.Server(pc, &tls.Config{
			NextProtos: []string{"h2", "http/1.1"},
			GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				return p.leaf(name)
			},
		})
		_ = tc.SetDeadline(time.Now().Add(30 * time.Second))
		if err := tc.Handshake(); err != nil {
			p.record(ProviderRequest{Host: host, Method: http.MethodConnect, At: at, Error: "tls: " + err.Error()})
			_ = tc.Close()
			return
		}
		_ = tc.SetDeadline(time.Time{})
		served = tc
	} else {
		host = hostOf(target, "80")
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { p.answer(w, r, host) }),
		ReadHeaderTimeout: 30 * time.Second}
	ln := newOneConnListener(served)
	srv.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed || s == http.StateHijacked {
			ln.done()
		}
	}
	_ = srv.Serve(ln)
}

// leaf is a certificate for host, signed by the run's CA, made once.
func (p *EgressProxy) leaf(host string) (*tls.Certificate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.leaves[host]; c != nil {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der, p.ca.Raw}, PrivateKey: key}
	p.leaves[host] = c
	return c, nil
}

// newEgressCA is a CA for one run, trusted by nothing but the builds it is handed to.
func newEgressCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "terma e2e egress CA", Organization: []string{"terma e2e"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(48 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	ca, err := x509.ParseCertificate(der)
	return ca, key, err
}

// hostOf is an authority without its scheme's default port.
func hostOf(authority, defaultPort string) string {
	if h, port, err := net.SplitHostPort(authority); err == nil && port == defaultPort {
		return h
	}
	return authority
}

// peekedConn is a hijacked connection read through the buffer that already holds its start.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// oneConnListener hands an http.Server one connection, then waits for it to close.
type oneConnListener struct {
	conn   net.Conn
	once   sync.Once
	closed chan struct{}
	taken  bool
	mu     sync.Mutex
}

func newOneConnListener(c net.Conn) *oneConnListener {
	return &oneConnListener{conn: c, closed: make(chan struct{})}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.taken {
		l.taken = true
		l.mu.Unlock()
		return l.conn, nil
	}
	l.mu.Unlock()
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConnListener) done()          { l.once.Do(func() { close(l.closed) }) }
func (l *oneConnListener) Close() error   { l.done(); return nil }
func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
