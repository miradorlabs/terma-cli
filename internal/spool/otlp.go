package spool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// OTLPSender delivers spooled events to the ingest host as OTLP/HTTP JSON log
// records — the same door a harness's own exporter uses, with the same project
// server key, so the backend needs nothing terma-specific to accept them. Each
// event becomes one log record named after the event (`terma.commit`, ...) with
// its attributes flattened onto the record.
type OTLPSender struct {
	// Endpoint is the OTLP base URL; /v1/logs is appended.
	Endpoint string
	// APIKey is the project's server key (ter_srv_…).
	APIKey    string
	ProjectID string
	Version   string
	HTTP      *http.Client
}

const (
	otlpLogsPath  = "/v1/logs"
	serviceName   = "terma-cli"
	sendTimeout   = 15 * time.Second
	attrProjectID = "mirador.project.id"
	attrSessionID = "session.id"
	attrRepo      = "terma.repo"
	attrEventName = "event.name"
	severityInfo  = 9
	severityText  = "INFO"
)

type otlpAttr struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	String *string  `json:"stringValue,omitempty"`
	Int    *string  `json:"intValue,omitempty"`
	Double *float64 `json:"doubleValue,omitempty"`
	Bool   *bool    `json:"boolValue,omitempty"`
}

func attrOf(key string, v any) otlpAttr {
	a := otlpAttr{Key: key}
	switch x := v.(type) {
	case string:
		a.Value.String = &x
	case bool:
		a.Value.Bool = &x
	case int:
		s := strconv.Itoa(x)
		a.Value.Int = &s
	case int64:
		s := strconv.FormatInt(x, 10)
		a.Value.Int = &s
	case float64:
		if x == float64(int64(x)) {
			s := strconv.FormatInt(int64(x), 10)
			a.Value.Int = &s
		} else {
			a.Value.Double = &x
		}
	default:
		s := fmt.Sprint(v)
		a.Value.String = &s
	}
	return a
}

// newHTTPClient builds the transport this sender delivers on, with redirects refused
// for the same reason internal/api does it: every request here carries the project's
// server key in an Authorization header. Go drops that header when a redirect
// crosses to another host, but keeps it on a same-host https→http downgrade, which
// would put a live server key on the wire in cleartext. An OTLP collector has no
// reason to redirect a log export, so a redirect here is a misconfiguration or an
// attack rather than a path worth following.
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: sendTimeout,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return fmt.Errorf("refusing to follow redirect to %s: the OTLP endpoint does not redirect log exports", req.URL.Redacted())
		},
	}
}

// Send implements Sender.
func (o *OTLPSender) Send(ctx context.Context, events []Event) ([]Event, error) {
	return nil, o.send(ctx, events)
}

func (o *OTLPSender) send(ctx context.Context, events []Event) error {
	if o.Endpoint == "" || o.APIKey == "" {
		return fmt.Errorf("spool flush needs an OTLP endpoint and a project key — run `terma install` in the repository")
	}
	records := make([]map[string]any, 0, len(events))
	for _, e := range events {
		attrs := []otlpAttr{attrOf(attrEventName, e.Name)}
		if e.SessionID != "" {
			attrs = append(attrs, attrOf(attrSessionID, e.SessionID))
		}
		if e.Repo != "" {
			attrs = append(attrs, attrOf(attrRepo, e.Repo))
		}
		for k, v := range e.Attrs {
			attrs = append(attrs, attrOf(k, v))
		}
		ns := strconv.FormatInt(e.Time.UnixNano(), 10)
		records = append(records, map[string]any{
			"timeUnixNano":         ns,
			"observedTimeUnixNano": ns,
			"severityNumber":       severityInfo,
			"severityText":         severityText,
			"eventName":            e.Name,
			"body":                 map[string]string{"stringValue": e.Name},
			"attributes":           attrs,
		})
	}
	payload := map[string]any{
		"resourceLogs": []map[string]any{{
			"resource": map[string]any{"attributes": []otlpAttr{
				attrOf("service.name", serviceName),
				attrOf("service.version", o.Version),
				attrOf(attrProjectID, o.ProjectID),
			}},
			"scopeLogs": []map[string]any{{
				"scope":      map[string]string{"name": serviceName, "version": o.Version},
				"logRecords": records,
			}},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	client := o.HTTP
	if client == nil {
		client = newHTTPClient()
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.Endpoint+otlpLogsPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.APIKey)
	req.Header.Set("User-Agent", serviceName+"/"+o.Version)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("otlp ingest returned %d: %s", resp.StatusCode, bytes.TrimSpace(detail))
	}
	return nil
}
