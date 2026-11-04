package e2e

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// proxyClient is an HTTP client through p, trusting its CA when trust is set.
func proxyClient(t *testing.T, p *EgressProxy, trust, h2 bool) *http.Client {
	t.Helper()
	proxyURL, _ := url.Parse(p.URL)
	pool := x509.NewCertPool()
	if trust {
		data, err := os.ReadFile(p.CAFile)
		if err != nil {
			t.Fatal(err)
		}
		pool.AppendCertsFromPEM(data)
	}
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: pool},
		ForceAttemptHTTP2: h2}}
}

// A client that trusts the run's CA reaches any HTTPS host through the proxy, over HTTP/1.1
// or HTTP/2, and plain HTTP too: each request is answered locally (the hosts do not exist), and
// recorded with its host, path, query and body, and no credential.
func TestEgressProxyOffline(t *testing.T) {
	p := StartEgressProxy(t)
	for _, h2 := range []bool{false, true} {
		var zipped bytes.Buffer
		zw := gzip.NewWriter(&zipped)
		zw.Write([]byte(`{"events":[1]}`))
		zw.Close()
		req, _ := http.NewRequest("POST", "https://telemetry.example.invalid/v1/rgstr?k=v", &zipped)
		req.Header.Set("Content-Encoding", "gzip")
		req.Header.Set("Authorization", "Bearer sk-secret")
		resp, err := proxyClient(t, p, true, h2).Do(req)
		if err != nil {
			t.Fatalf("h2=%v: %v", h2, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "{}" {
			t.Errorf("h2=%v: answered %d %q", h2, resp.StatusCode, body)
		}
		if h2 && resp.ProtoMajor != 2 {
			t.Errorf("an HTTP/2 client spoke %s", resp.Proto)
		}
	}
	resp, err := proxyClient(t, p, true, false).Get("http://plain.example.invalid:8080/check")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	reqs := p.Requests()
	if len(reqs) != 3 {
		t.Fatalf("recorded %d requests, want 3: %+v", len(reqs), reqs)
	}
	for _, r := range reqs[:2] {
		if r.Host != "telemetry.example.invalid" || r.Method != "POST" || r.Path != "/v1/rgstr" || r.Query != "k=v" ||
			string(r.Body) != `{"events":[1]}` || r.Status != 200 || r.At.IsZero() {
			t.Errorf("recorded %+v", r)
		}
		if got := r.Header["authorization"]; len(got) != 1 || got[0] != "[redacted]" {
			t.Errorf("authorization kept as %q", got)
		}
		if r.When != "" {
			t.Errorf("a run with no model call placed a request %q", r.When)
		}
	}
	if r := reqs[2]; r.Host != "plain.example.invalid:8080" || r.Method != "GET" || r.Path != "/check" {
		t.Errorf("plain request recorded as %+v", r)
	}
	data, _ := json.Marshal(reqs)
	if strings.Contains(string(data), "sk-secret") {
		t.Errorf("a credential reached the record: %s", data)
	}
}

// A client that does not trust the CA (a pinned host) fails, and the proxy records the host.
func TestEgressProxyPinnedOffline(t *testing.T) {
	p := StartEgressProxy(t)
	if _, err := proxyClient(t, p, false, false).Get("https://pinned.example.invalid/"); err == nil {
		t.Fatal("a client that does not trust the CA was answered")
	}
	// The proxy's side of the handshake ends when the client's alert arrives.
	var reqs []ProviderRequest
	for deadline := time.Now().Add(5 * time.Second); len(reqs) == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		reqs = p.Requests()
	}
	if len(reqs) != 1 || reqs[0].Method != "CONNECT" || reqs[0].Host != "pinned.example.invalid" || !strings.HasPrefix(reqs[0].Error, "tls: ") {
		t.Fatalf("recorded %+v", reqs)
	}
}

// Requests are placed before, during and after the model calls the provider marks; Record puts
// them in the profile, with the proxy's literals as placeholders.
func TestEgressProxyWhenOffline(t *testing.T) {
	p := StartEgressProxy(t)
	client := proxyClient(t, p, true, false)
	get := func(path string) {
		t.Helper()
		resp, err := client.Get("https://flags.example.invalid" + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	provider := httptest.NewServer(p.Provider(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			get("/during")
		}
		io.WriteString(w, "{}")
	})))
	defer provider.Close()
	get("/before")
	if resp, err := http.Get(provider.URL + "/not-a-model-call"); err == nil {
		resp.Body.Close()
	}
	resp, err := http.Post(provider.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	get("/after")
	b := Binary{Harness: "claude", Version: "9.9.8"}
	p.Record(t, b)
	prof := profileOf(t, b)
	var got []string
	for _, r := range prof.Egress {
		got = append(got, r.Path+"="+r.When)
	}
	if strings.Join(got, " ") != "/before=before /during=during /after=after" {
		t.Errorf("egress placed %v", got)
	}
	if prof.Placeholders[p.URL] != "<proxy>" || prof.Placeholders[p.CAFile] != "<proxy-ca>" {
		t.Errorf("placeholders = %v", prof.Placeholders)
	}
}

// Env points every runtime at the proxy and its CA, and keeps loopback off it.
func TestEgressProxyEnvOffline(t *testing.T) {
	p := StartEgressProxy(t)
	env := strings.Join(p.Env(), "\n")
	for _, want := range []string{"HTTPS_PROXY=" + p.URL, "https_proxy=" + p.URL, "NO_PROXY=127.0.0.1,localhost,::1",
		"NODE_EXTRA_CA_CERTS=" + p.CAFile, "CODEX_CA_CERTIFICATE=" + p.CAFile, "SSL_CERT_FILE=" + p.CAFile, "NODE_USE_ENV_PROXY=1"} {
		if !strings.Contains(env, want) {
			t.Errorf("Env lacks %s", want)
		}
	}
}
