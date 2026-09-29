package relay

import (
	"compress/gzip"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"time"
)

// maxRequestBytes bounds one export, compressed or not. It matches the gateway's own
// limit, so nothing the relay accepts is refused downstream for its size.
const maxRequestBytes = 16 << 20

// intake is the relay's HTTP face: OTLP/HTTP on /v1/{logs,traces,metrics}, health on
// /healthz. It writes each accepted body to the inbox before answering, so an agent's
// export is on disk the moment the agent is told it was received.
type intake struct {
	dir     string
	token   string
	version string
	now     func() time.Time
	stats   *stats
	logf    func(string, ...any)
	// accepted wakes the router.
	accepted func()
}

func (in *intake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/logs", in.export)
	mux.HandleFunc("POST /v1/traces", in.export)
	mux.HandleFunc("POST /v1/metrics", in.export)
	mux.HandleFunc("GET /healthz", in.health)
	return mux
}

func (in *intake) authorized(r *http.Request) bool {
	want := "Bearer " + in.token
	got := r.Header.Get("Authorization")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (in *intake) export(w http.ResponseWriter, r *http.Request) {
	if !in.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sig, _ := signalOfPath(r.URL.Path)
	body, err := readBody(w, r)
	if err != nil {
		status := http.StatusBadRequest
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return
	}
	format := formatJSON
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "application/x-protobuf" || mt == "application/protobuf" ||
		(mt == "" && !looksLikeJSON(body)) {
		format = formatProto
	}
	e := newEntry(in.now(), sig, format)
	if err := writeEntry(filepath.Join(in.dir, inboxDir), e, body); err != nil {
		in.logf("accept %s: %v", sig, err)
		// 503 is retryable for every OTLP exporter: the agent keeps the batch.
		http.Error(w, "relay could not store the export", http.StatusServiceUnavailable)
		return
	}
	in.stats.addReceived(1)
	in.accepted()
	if format == formatJSON {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

// readBody reads the request body, gunzipping it when the exporter compressed it, never
// past maxRequestBytes either way.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	var src io.Reader = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	switch r.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(src)
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		src = io.LimitReader(zr, maxRequestBytes+1)
	default:
		return nil, errors.New("unsupported content encoding")
	}
	body, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	if len(body) > maxRequestBytes {
		return nil, &http.MaxBytesError{Limit: maxRequestBytes}
	}
	return body, nil
}

func (in *intake) health(w http.ResponseWriter, r *http.Request) {
	if !in.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	counters, held, lastError := in.stats.snapshot()
	report := Health{
		Version:      in.version,
		Backlog:      outboxBacklog(in.dir),
		Placing:      len(collect(filepath.Join(in.dir, inboxDir))),
		HeldProjects: held,
		LastError:    lastError,
		Counters:     counters,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(report)
}
