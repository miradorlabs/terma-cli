package relay

import (
	"encoding/json"
	"net/http"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// SensorService is the service.name of the records the sensor's rollups become.
const SensorService = "terma-sensor"

// SensorRollup is one session's window from `terma sensor`, by its root process: the
// agent's own process, which the relay knows from the telemetry that process exports.
type SensorRollup struct {
	Root       int               `json:"root"`
	End        time.Time         `json:"end"`
	Window     int               `json:"window"` // seconds
	Network    []SensorTraffic   `json:"network,omitempty"`
	FilesRead  int64             `json:"files_read"`
	FilesWrote int64             `json:"files_written"`
	Removed    int               `json:"removed"`
	Processes  int               `json:"processes"`
	Blocks     []SensorBlock     `json:"blocks,omitempty"`
	Containers []SensorContainer `json:"containers,omitempty"`
}

// SensorTraffic is a session's traffic to one host in a window.
type SensorTraffic struct {
	Host        string `json:"host"`
	Sent        int64  `json:"sent"`
	Received    int64  `json:"received"`
	Connections int    `json:"connections"`
}

// SensorBlock is a policy rule's denial; its target stays on the machine.
type SensorBlock struct {
	Kind     string    `json:"kind"`
	Rule     int       `json:"rule"`
	Enforced bool      `json:"enforced"`
	At       time.Time `json:"at"`
}

// SensorContainer is a container the session started.
type SensorContainer struct {
	Name  string    `json:"name"`
	Image string    `json:"image"`
	At    time.Time `json:"at"`
}

// SensorReply says which roots the relay placed; the sensor keeps the others' rollups and
// sends them again with the next window.
type SensorReply struct {
	Placed   []int `json:"placed"`
	Unplaced []int `json:"unplaced"`
}

// sensor takes the sensor's rollups. A rollup goes where its root's own telemetry went:
// the one session that process named, under that session's claim, policy and key. A root
// that named none yet, or several (a shared daemon), is not placed: the sensor keeps it.
func (r *Relay) sensor(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !r.authorized(req) {
		r.stats.add("refused_unauthorized", 1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var rollups []SensorRollup
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxBody)).Decode(&rollups); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	reply := SensorReply{Placed: []int{}, Unplaced: []int{}}
	for _, ro := range rollups {
		session, ok := r.onlySessionOf(ro.Root)
		if !ok {
			reply.Unplaced = append(reply.Unplaced, ro.Root)
			continue
		}
		msg, n := sensorLogs(session, ro, r.opts.Version)
		if n == 0 {
			reply.Placed = append(reply.Placed, ro.Root)
			continue
		}
		r.stats.received(Logs, n)
		r.route(&part{signal: Logs, session: session, msg: msg, records: n, pid: ro.Root, at: ro.End})
		reply.Placed = append(reply.Placed, ro.Root)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

// onlySessionOf is the one session pid's own telemetry named.
func (r *Relay) onlySessionOf(pid int) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.procs[pid]
	if st == nil || st.overflow || len(st.sessions) != 1 {
		return "", false
	}
	for s := range st.sessions {
		return s, true
	}
	return "", false
}

// sensorLogs is a rollup as log records, and how many.
func sensorLogs(session string, ro SensorRollup, version string) (*logspb.LogsData, int) {
	t := uint64(ro.End.UnixNano())
	rec := func(event string, attrs ...*commonpb.KeyValue) *logspb.LogRecord {
		return &logspb.LogRecord{
			TimeUnixNano: t, ObservedTimeUnixNano: t, EventName: event, Body: strValue(event),
			SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_INFO, SeverityText: "INFO",
			Attributes: append([]*commonpb.KeyValue{{Key: semconv.SessionIDKey, Value: strValue(session)}}, attrs...),
		}
	}
	window := intKV(semconv.TermaSensorWindowKey, int64(ro.Window))
	var recs []*logspb.LogRecord
	for _, n := range ro.Network {
		recs = append(recs, rec(semconv.TermaSensorNetworkEvent,
			&commonpb.KeyValue{Key: semconv.ServerAddressKey, Value: strValue(n.Host)}, window,
			intKV(semconv.TermaSensorNetworkSentKey, n.Sent), intKV(semconv.TermaSensorNetworkReceivedKey, n.Received),
			intKV(semconv.TermaSensorNetworkConnectionsKey, int64(n.Connections))))
	}
	if ro.FilesRead+ro.FilesWrote > 0 || ro.Removed+ro.Processes > 0 {
		recs = append(recs, rec(semconv.TermaSensorFilesEvent, window,
			intKV(semconv.TermaSensorFilesReadKey, ro.FilesRead), intKV(semconv.TermaSensorFilesWrittenKey, ro.FilesWrote),
			intKV(semconv.TermaSensorFilesRemovedKey, int64(ro.Removed)), intKV(semconv.TermaSensorProcessesKey, int64(ro.Processes))))
	}
	for _, b := range ro.Blocks {
		r := rec(semconv.TermaSensorBlockEvent,
			&commonpb.KeyValue{Key: semconv.TermaSensorBlockKindKey, Value: strValue(b.Kind)},
			intKV(semconv.TermaSensorBlockRuleKey, int64(b.Rule)),
			&commonpb.KeyValue{Key: semconv.TermaSensorBlockEnforcedKey, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: b.Enforced}}})
		r.TimeUnixNano = uint64(b.At.UnixNano())
		recs = append(recs, r)
	}
	for _, c := range ro.Containers {
		r := rec(semconv.TermaSensorContainerEvent,
			&commonpb.KeyValue{Key: semconv.ContainerNameKey, Value: strValue(c.Name)},
			&commonpb.KeyValue{Key: semconv.ContainerImageNameKey, Value: strValue(c.Image)})
		r.TimeUnixNano = uint64(c.At.UnixNano())
		recs = append(recs, r)
	}
	if len(recs) == 0 {
		return nil, 0
	}
	res := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		{Key: semconv.ServiceNameKey, Value: strValue(SensorService)},
		{Key: semconv.ServiceVersionKey, Value: strValue(version)},
		{Key: semconv.TermaSchemaVersionKey, Value: strValue(semconv.SchemaVersion)},
	}}
	return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: res, ScopeLogs: []*logspb.ScopeLogs{{
		Scope: &commonpb.InstrumentationScope{Name: SensorService, Version: version}, LogRecords: recs,
	}}}}}, len(recs)
}

func intKV(key string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}
