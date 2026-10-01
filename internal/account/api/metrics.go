package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

const metricsQueryPath = "/v1/metrics/query"

// PromSample is one series of an instant-query vector: its labels and the value at
// the evaluation instant.
type PromSample struct {
	Metric map[string]string `json:"metric"`
	Value  PromValue         `json:"value"`
}

// PromValue is Prometheus's [unix_seconds, "value"] pair. The value travels as a
// string so precision survives; Float parses it on demand.
type PromValue struct {
	Time  float64
	Value string
}

// UnmarshalJSON reads the two-element array, refusing any other length: a sample with
// a missing half would otherwise decode as a zero at time zero.
func (v *PromValue) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw) != 2 {
		return fmt.Errorf("metric value has %d parts, want [time, value]", len(raw))
	}
	if err := json.Unmarshal(raw[0], &v.Time); err != nil {
		return fmt.Errorf("metric value time: %w", err)
	}
	if err := json.Unmarshal(raw[1], &v.Value); err != nil {
		return fmt.Errorf("metric value: %w", err)
	}
	return nil
}

// MarshalJSON writes the pair back in Prometheus's shape, so `--output json` prints
// what the gateway sent.
func (v PromValue) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{v.Time, v.Value})
}

// Float parses the sample's value. NaN and Inf are legal Prometheus values and parse.
func (s PromSample) Float() (float64, error) {
	return strconv.ParseFloat(s.Value.Value, 64)
}

// PromResult is the Prometheus /api/v1/query envelope the gateway mirrors.
type PromResult struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string       `json:"resultType"`
		Result     []PromSample `json:"result"`
	} `json:"data"`
	ErrorType string `json:"errorType,omitempty"`
	Error     string `json:"error,omitempty"`
}

// QueryMetric evaluates a PromQL expression at one instant (zero at means now). The
// gateway answers a bad expression with a 400, which surfaces as an *APIError; a
// query that matches nothing is an empty vector, not an error.
func (c *Client) QueryMetric(ctx context.Context, expr string, at time.Time) (PromResult, error) {
	q := url.Values{"query": {expr}}
	if !at.IsZero() {
		q.Set("time", at.UTC().Format(time.RFC3339Nano))
	}
	var out PromResult
	if err := c.Get(ctx, metricsQueryPath, q, &out); err != nil {
		return out, err
	}
	if out.Status != "" && out.Status != "success" {
		return out, fmt.Errorf("metric query failed: %s %s", out.ErrorType, out.Error)
	}
	return out, nil
}
