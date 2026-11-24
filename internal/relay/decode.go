package relay

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func readBody(req *http.Request) ([]byte, error) {
	var rd io.Reader = http.MaxBytesReader(nil, req.Body, maxBody)
	if strings.EqualFold(req.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(rd)
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		rd = io.LimitReader(zr, maxBody+1)
	}
	body, err := io.ReadAll(rd)
	if err == nil && len(body) > maxBody {
		err = fmt.Errorf("export larger than %d bytes", maxBody)
	}
	return body, err
}

func (r *Relay) decode(s Signal, body []byte, isJSON bool) (map[string]*part, error) {
	unmarshal := proto.Unmarshal
	if isJSON {
		fixed, err := hexIDsToBase64(body)
		if err != nil {
			return nil, err
		}
		body = fixed
		unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal
	}
	switch s {
	case Logs:
		var m logspb.LogsData
		if err := unmarshal(body, &m); err != nil {
			return nil, err
		}
		return r.rules.splitLogs(&m, r.learnTrace), nil
	case Traces:
		var m tracepb.TracesData
		if err := unmarshal(body, &m); err != nil {
			return nil, err
		}
		return r.rules.splitTraces(&m, r.learnTrace, r.traceOf), nil
	default:
		var m metricspb.MetricsData
		if err := unmarshal(body, &m); err != nil {
			return nil, err
		}
		return r.rules.splitMetrics(&m), nil
	}
}
