package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// Helpers shared by the flow journeys (*_flow_e2e_test.go). Each journey
// drives the real router over HTTP against a real Postgres and checks what
// the API answers, the way a dashboard page or an integrator would see it.

// flowReq is one request: a bearer key OR a session cookie, optional extra
// headers, and a body (JSON-encoded, or sent as is when it is []byte).
type flowReq struct {
	bearer string
	cookie *http.Cookie
	header map[string]string
	body   any
}

type flowResp struct {
	status int
	body   map[string]any
	raw    []byte
}

func flowDo(t *testing.T, ts *httptest.Server, method, path string, req flowReq) flowResp {
	t.Helper()
	var rdr io.Reader = http.NoBody
	switch b := req.body.(type) {
	case nil:
	case []byte:
		rdr = bytes.NewReader(b)
	default:
		enc, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("encode %s %s body: %v", method, path, err)
		}
		rdr = bytes.NewReader(enc)
	}
	r, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	r.Header.Set("Content-Type", "application/json")
	if req.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+req.bearer)
	}
	if req.cookie != nil {
		r.AddCookie(req.cookie)
	}
	for k, v := range req.header {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return flowResp{status: resp.StatusCode, body: body, raw: raw}
}

// want stops the test unless the response has the expected status.
func (r flowResp) want(t *testing.T, status int, what string) flowResp {
	t.Helper()
	if r.status != status {
		t.Fatalf("%s: status %d, want %d; body: %s", what, r.status, status, r.raw)
	}
	return r
}

// errCode is the error envelope's code ({"error": {"code": ...}}).
func (r flowResp) errCode() string { return flowStr(r.body, "error", "code") }

// errMessage is the error envelope's message.
func (r flowResp) errMessage() string { return flowStr(r.body, "error", "message") }

func flowObj(m map[string]any, path ...string) map[string]any {
	cur := m
	for _, k := range path {
		next, _ := cur[k].(map[string]any)
		cur = next
	}
	return cur
}

func flowStr(m map[string]any, path ...string) string {
	if len(path) == 0 {
		return ""
	}
	v, _ := flowObj(m, path[:len(path)-1]...)[path[len(path)-1]].(string)
	return v
}

func flowInt(m map[string]any, path ...string) int64 {
	if len(path) == 0 {
		return 0
	}
	v, _ := flowObj(m, path[:len(path)-1]...)[path[len(path)-1]].(float64)
	return int64(v)
}

func flowList(m map[string]any, key string) []map[string]any {
	items, _ := m[key].([]any)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if o, ok := it.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

// flowBootstrap creates the account and its owner through POST
// /v1/bootstrap (VELOX_BOOTSTRAP_TOKEN must be set before NewServer) and
// returns the body: tenant, secret_key_test, secret_key_live.
func flowBootstrap(t *testing.T, ts *httptest.Server, token, email, password string) map[string]any {
	t.Helper()
	return flowDo(t, ts, "POST", "/v1/bootstrap", flowReq{body: map[string]any{
		"token":          token,
		"tenant_name":    "Flow " + email,
		"owner_email":    email,
		"owner_password": password,
	}}).want(t, 201, "bootstrap").body
}

// flowCount runs a COUNT query outside row-level security.
func flowCount(t *testing.T, db *postgres.DB, query string, args ...any) int {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// flowEmailLinkToken reads the newest email_outbox row of emailType and
// returns the token= value of the link in its payload field. The link
// travels only by email; the outbox row is the payload the dispatcher
// would send.
func flowEmailLinkToken(t *testing.T, db *postgres.DB, emailType, field string) string {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	var link string
	if err := tx.QueryRowContext(t.Context(), `
		SELECT COALESCE(payload->>$2, '') FROM email_outbox
		WHERE email_type = $1
		ORDER BY created_at DESC, id DESC LIMIT 1`, emailType, field).Scan(&link); err != nil {
		t.Fatalf("read %s email from the outbox: %v", emailType, err)
	}
	_, token, found := strings.Cut(link, "token=")
	if !found || token == "" {
		t.Fatalf("no token in the %s email's %s %q", emailType, field, link)
	}
	return token
}

// signedStripeEvent encodes a Stripe event and signs it the way Stripe
// does (Stripe-Signature header, v1 scheme, current timestamp).
func signedStripeEvent(t *testing.T, secret string, event map[string]any) ([]byte, string) {
	t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("encode stripe event: %v", err)
	}
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: secret})
	return payload, signed.Header
}

// stripeStub replaces api.stripe.com for the rest of the test. It answers
// the calls these journeys make (create a customer, create a Checkout
// session, read a PaymentIntent with its card, read or list a saved card,
// set a customer's default card) and records every call.
// Anything else gets a 404 and is listed by unexpected().
type stripeStub struct {
	mu    sync.Mutex
	calls []string              // "POST /v1/customers"
	forms map[string]url.Values // last form sent to each "METHOD path"
}

func installStripeStub(t *testing.T) *stripeStub {
	t.Helper()
	stub := &stripeStub{forms: map[string]url.Values{}}
	prev := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.stripe.com" {
			return prev.RoundTrip(req)
		}
		return stub.answer(req), nil
	})
	t.Cleanup(func() { http.DefaultTransport = prev })
	return stub
}

func (s *stripeStub) answer(req *http.Request) *http.Response {
	form := req.URL.Query()
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		if parsed, err := url.ParseQuery(string(b)); err == nil {
			for k, v := range parsed {
				form[k] = v
			}
		}
	}
	call := req.Method + " " + req.URL.Path
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.forms[call] = form
	s.mu.Unlock()

	status, body := 200, ""
	switch {
	case call == "POST /v1/customers":
		body = `{"id":"cus_flow","object":"customer"}`
	case call == "POST /v1/checkout/sessions":
		body = `{"id":"cs_flow","object":"checkout.session","mode":"setup","url":"https://checkout.stripe.test/c/cs_flow"}`
	case req.Method == "POST" && strings.HasPrefix(req.URL.Path, "/v1/customers/"):
		body = fmt.Sprintf(`{"id":%q,"object":"customer"}`, strings.TrimPrefix(req.URL.Path, "/v1/customers/"))
	case call == "GET /v1/payment_methods":
		body = `{"object":"list","url":"/v1/payment_methods","has_more":false,"data":[` + stubCard + `]}`
	case req.Method == "GET" && strings.HasPrefix(req.URL.Path, "/v1/payment_methods/"):
		body = stubCard
	case req.Method == "GET" && strings.HasPrefix(req.URL.Path, "/v1/payment_intents/"):
		id := strings.TrimPrefix(req.URL.Path, "/v1/payment_intents/")
		body = fmt.Sprintf(`{"id":%q,"object":"payment_intent","status":"succeeded","payment_method":{"id":"pm_flow","object":"payment_method","type":"card","card":{"brand":"visa","display_brand":"visa","last4":"4242","exp_month":12,"exp_year":2030}}}`, id)
	default:
		status = 404
		body = fmt.Sprintf(`{"error":{"type":"invalid_request_error","message":"stripe stub has no answer for %s"}}`, call)
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

// stubCard is the saved card the stub answers payment-method reads with.
const stubCard = `{"id":"pm_flow","object":"payment_method","type":"card","created":1767225600,"card":{"brand":"visa","display_brand":"visa","last4":"4242","exp_month":12,"exp_year":2030,"fingerprint":"fp_flow"}}`

func (s *stripeStub) count(call string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c == call {
			n++
		}
	}
	return n
}

func (s *stripeStub) form(call string) url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forms[call]
}

func (s *stripeStub) unexpected() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		if c != "POST /v1/customers" && c != "POST /v1/checkout/sessions" && !strings.HasPrefix(c, "GET /v1/payment_intents/") &&
			!strings.HasPrefix(c, "GET /v1/payment_methods") && !strings.HasPrefix(c, "POST /v1/customers/") {
			out = append(out, c)
		}
	}
	return out
}
