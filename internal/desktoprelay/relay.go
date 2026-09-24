package desktoprelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/shim"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Address is the fixed loopback receiver configured in Codex's user-level otel
// table. Only logs are routed: native metric points have no conversation id.
const Address = "127.0.0.1:43199"

// Endpoint is the user-level Codex OTLP base URL for the local relay.
const Endpoint = "http://" + Address

const (
	maxRequestBytes  = 16 << 20
	flushInterval    = 10 * time.Second
	sendTimeout      = 15 * time.Second
	registrationWait = time.Second
)

// Relay accepts Codex OTLP logs locally, splits a mixed batch by registered
// conversation, and durably queues each project's records for delivery.
type Relay struct {
	queue    *Queue
	client   *http.Client
	flush    chan struct{}
	accepted atomic.Uint64
	dropped  atomic.Uint64
}

// New constructs a relay with a private on-disk delivery queue.
func New() (*Relay, error) {
	queue, err := OpenQueue()
	if err != nil {
		return nil, err
	}
	return &Relay{queue: queue, client: &http.Client{Timeout: sendTimeout}, flush: make(chan struct{}, 1)}, nil
}

// Handler is the loopback OTLP/HTTP endpoint. It acknowledges a batch only after
// every routable project portion has been queued durably.
func (r *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/logs", r.handleLogs)
	mux.HandleFunc("GET /health", r.health)
	return mux
}

func (r *Relay) health(w http.ResponseWriter, _ *http.Request) {
	count, size, err := r.queue.Pending()
	if err != nil {
		http.Error(w, "queue unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ready": true, "queued_batches": count, "queued_bytes": size,
		"accepted_records": r.accepted.Load(), "unrouted_records": r.dropped.Load(),
	})
}

func (r *Relay) handleLogs(w http.ResponseWriter, req *http.Request) {
	contentType := req.Header.Get("Content-Type")
	if !strings.Contains(contentType, "protobuf") && !strings.Contains(contentType, "json") {
		http.Error(w, "OTLP protobuf or JSON required", http.StatusUnsupportedMediaType)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxRequestBytes))
	if err != nil {
		http.Error(w, "OTLP request too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	var incoming collogspb.ExportLogsServiceRequest
	if strings.Contains(contentType, "json") {
		err = protojson.Unmarshal(data, &incoming)
	} else {
		err = proto.Unmarshal(data, &incoming)
	}
	if err != nil {
		http.Error(w, "invalid OTLP logs", http.StatusBadRequest)
		return
	}
	// Codex can export its first records while SessionStart is still running.
	// Wait briefly for that trusted hook; never guess a project from the payload.
	deadline := time.Now().Add(registrationWait)
	for hasUnregisteredSession(&incoming, time.Now()) && time.Now().Before(deadline) {
		select {
		case <-req.Context().Done():
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
	outputs, accepted, dropped := splitByProject(&incoming, time.Now())
	encoded := make(map[string][]byte, len(outputs))
	for projectID, batch := range outputs {
		body, err := proto.Marshal(batch)
		if err != nil {
			http.Error(w, "encode OTLP logs", http.StatusInternalServerError)
			return
		}
		encoded[projectID] = body
	}
	if len(encoded) > 0 {
		if err := r.queue.EnqueueMany(encoded); err != nil {
			http.Error(w, "desktop relay queue unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	r.accepted.Add(uint64(accepted))
	r.dropped.Add(uint64(dropped))
	if accepted > 0 {
		select {
		case r.flush <- struct{}{}:
		default:
		}
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	body, _ := proto.Marshal(&collogspb.ExportLogsServiceResponse{})
	_, _ = w.Write(body)
}

func hasUnregisteredSession(in *collogspb.ExportLogsServiceRequest, now time.Time) bool {
	for _, resource := range in.GetResourceLogs() {
		for _, scope := range resource.GetScopeLogs() {
			for _, record := range scope.GetLogRecords() {
				id := conversationID(record.GetAttributes())
				if id == "" {
					id = conversationID(resource.GetResource().GetAttributes())
				}
				if id != "" && ProjectFor(id, now) == "" {
					return true
				}
			}
		}
	}
	return false
}

// splitByProject preserves every OTLP resource, scope, and log field while
// separating records by the project registered for conversation.id. Native
// records without a usable conversation id or configured route stay local.
func splitByProject(in *collogspb.ExportLogsServiceRequest, now time.Time) (map[string]*collogspb.ExportLogsServiceRequest, int, int) {
	out := map[string]*collogspb.ExportLogsServiceRequest{}
	var accepted, dropped int
	for _, resource := range in.GetResourceLogs() {
		for _, scope := range resource.GetScopeLogs() {
			grouped := map[string][]*logspb.LogRecord{}
			for _, record := range scope.GetLogRecords() {
				id := conversationID(record.GetAttributes())
				if id == "" {
					id = conversationID(resource.GetResource().GetAttributes())
				}
				projectID := ProjectFor(id, now)
				route, ready := routePolicy(projectID)
				if projectID == "" || !ready || !recordAllowed(record, route) {
					dropped++
					continue
				}
				grouped[projectID] = append(grouped[projectID], record)
				accepted++
			}
			for projectID, records := range grouped {
				resourceCopy := proto.Clone(resource).(*logspb.ResourceLogs)
				resourceCopy.ScopeLogs = nil
				scopeCopy := proto.Clone(scope).(*logspb.ScopeLogs)
				scopeCopy.LogRecords = records
				resourceCopy.ScopeLogs = append(resourceCopy.ScopeLogs, scopeCopy)
				if out[projectID] == nil {
					out[projectID] = &collogspb.ExportLogsServiceRequest{}
				}
				out[projectID].ResourceLogs = append(out[projectID].ResourceLogs, resourceCopy)
			}
		}
	}
	return out, accepted, dropped
}

func conversationID(attrs []*commonpb.KeyValue) string {
	for _, attr := range attrs {
		if attr.GetKey() == "conversation.id" {
			return attr.GetValue().GetStringValue()
		}
	}
	return ""
}

func routePolicy(projectID string) (shim.Record, bool) {
	if projectID == "" {
		return shim.Record{}, false
	}
	rec, ok, err := shim.LoadRecord(projectID)
	ready := err == nil && ok && rec.ProjectID == projectID &&
		(rec.Desktop == nil || *rec.Desktop) &&
		slices.Contains(rec.Harnesses, shim.AgentCodex) && slices.Contains(rec.Signals, "logs") &&
		keystore.GetFor(shim.AgentCodex, projectID) != ""
	return rec, ready
}

// ReadyForProject reports whether desktop logs for this project have a Codex
// route and key. It does not imply the repository hooks have been trusted.
func ReadyForProject(projectID string) bool {
	_, ready := routePolicy(projectID)
	return ready
}

// Codex emits tool arguments even when tool_result.max_bytes is zero. With a
// repository's content switch off, dropping the whole result is the safe policy;
// the separate tool decision still records its name and authorization outcome.
func recordAllowed(record *logspb.LogRecord, rec shim.Record) bool {
	switch eventName(record.GetAttributes()) {
	case "codex.user_prompt":
		return rec.IncludePrompts
	case "codex.tool_result":
		return rec.IncludeToolContent
	default:
		return true
	}
}

func eventName(attrs []*commonpb.KeyValue) string {
	for _, attr := range attrs {
		if attr.GetKey() == "event.name" {
			return attr.GetValue().GetStringValue()
		}
	}
	return ""
}

func destination(projectID string) (string, string, error) {
	rec, ok, err := shim.LoadRecord(projectID)
	if err != nil || !ok || rec.ProjectID != projectID ||
		(rec.Desktop != nil && !*rec.Desktop) ||
		!slices.Contains(rec.Harnesses, shim.AgentCodex) || !slices.Contains(rec.Signals, "logs") {
		return "", "", errors.New("codex logs are no longer routed for this project")
	}
	key := keystore.GetFor(shim.AgentCodex, projectID)
	if key == "" {
		return "", "", errors.New("codex project key is unavailable")
	}
	u, err := url.Parse(rec.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Host == Address {
		return "", "", errors.New("invalid desktop relay destination")
	}
	return strings.TrimRight(rec.Endpoint, "/") + "/v1/logs", key, nil
}

func (r *Relay) send(ctx context.Context, projectID string, payload []byte) error {
	endpoint, key, err := destination(projectID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("terma OTLP ingest returned HTTP %d", res.StatusCode)
	}
	return nil
}

// Run listens only on loopback and retries durable deliveries in the background.
func (r *Relay) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", Address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: r.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	go r.flushLoop(ctx)
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (r *Relay) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		_, _ = r.queue.Drain(ctx, r.send)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-r.flush:
		}
	}
}
