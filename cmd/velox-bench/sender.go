package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/usage"
)

// sender is one way to deliver a batch of events. Two implementations exist so
// the pacing, the coordinated-omission correction, and the reporting are
// literally the same code for both — any difference in the numbers is a
// difference in the transport, not in how it was measured.
type sender interface {
	// send delivers n events and reports how many were accepted.
	send(ctx context.Context, events []event) (accepted int, err error)
	describe() string
}

// event is transport-neutral: the in-process sender turns it into an
// IngestInput, the HTTP sender marshals it to the public JSON shape.
type event struct {
	quantity   decimal.Decimal
	dimensions map[string]any
}

// --- in-process ---------------------------------------------------------

type inProcessSender struct {
	svc                  *usage.Service
	tenantID, customerID string
	meterID              string
}

func (s *inProcessSender) describe() string {
	return "in-process (usage.Service — excludes router, auth, JSON decode)"
}

func (s *inProcessSender) send(ctx context.Context, events []event) (int, error) {
	if len(events) == 1 {
		if _, err := s.svc.Ingest(ctx, s.tenantID, usage.IngestInput{
			CustomerID: s.customerID, MeterID: s.meterID,
			Quantity: events[0].quantity, Dimensions: events[0].dimensions,
		}); err != nil {
			return 0, err
		}
		return 1, nil
	}
	inputs := make([]usage.IngestInput, len(events))
	for i, e := range events {
		inputs[i] = usage.IngestInput{
			CustomerID: s.customerID, MeterID: s.meterID,
			Quantity: e.quantity, Dimensions: e.dimensions,
		}
	}
	inserted, _, errs := s.svc.BatchIngest(ctx, s.tenantID, inputs)
	if len(errs) > 0 {
		return inserted, errs[0]
	}
	return inserted, nil
}

// --- HTTP ---------------------------------------------------------------

// httpSender drives the public ingest endpoint the way a customer's SDK does:
// POST /v1/usage-events (or /batch), Bearer auth, JSON in and out. This is the
// only mode whose numbers are end-to-end — it includes the router, the auth
// middleware, JSON decode, and the customer/meter resolution the in-process
// path is handed for free.
type httpSender struct {
	client             *http.Client
	baseURL, apiKey    string
	externalCustomerID string
	eventName          string
}

func (s *httpSender) describe() string {
	return "HTTP " + s.baseURL + "/v1/usage-events (end-to-end: router + auth + JSON + resolve)"
}

type wireEvent struct {
	ExternalCustomerID string          `json:"external_customer_id"`
	EventName          string          `json:"event_name"`
	Quantity           decimal.Decimal `json:"quantity"`
	Dimensions         map[string]any  `json:"dimensions,omitempty"`
}

func (s *httpSender) send(ctx context.Context, events []event) (int, error) {
	wire := make([]wireEvent, len(events))
	for i, e := range events {
		wire[i] = wireEvent{
			ExternalCustomerID: s.externalCustomerID,
			EventName:          s.eventName,
			Quantity:           e.quantity,
			Dimensions:         e.dimensions,
		}
	}

	url := s.baseURL + "/v1/usage-events"
	var body []byte
	var err error
	if len(events) == 1 {
		body, err = json.Marshal(wire[0])
	} else {
		url += "/batch"
		body, err = json.Marshal(wire)
	}
	if err != nil {
		return 0, fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain before close so the connection is returned to the idle pool
	// instead of being torn down. Skipping this turns a keep-alive benchmark
	// into a TCP-handshake benchmark.
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(payload), 200))
	}
	return len(events), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// newHTTPClient returns a client tuned so the benchmark measures ingest rather
// than connection setup. Go's default MaxIdleConnsPerHost is 2: with more
// workers than that, most requests would open a fresh TCP connection and the
// run would report handshake cost as if it were server latency.
func newHTTPClient(workers int) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConns:        workers * 2,
			MaxIdleConnsPerHost: workers * 2,
			MaxConnsPerHost:     workers * 2,
			IdleConnTimeout:     90 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}
}
