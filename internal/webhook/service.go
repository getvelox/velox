package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/platform/scheduler"
)

// maxRetries is how many times a failed delivery is retried after its
// initial attempt — one per slot in retryBackoffs. Total attempts =
// 1 initial + maxRetries (so 6 with the current ladder).
//
// Was named maxAttempts and compared with `>=`, which capped the delivery
// at maxAttempts TOTAL attempts and so left the final backoff slot
// (retryBackoffs[maxRetries-1] = 24h) permanently unreachable: a delivery
// gave up after attempt 5 / ~2.5h instead of running the full ~26.5h
// ladder the 24h tail implies.
const maxRetries = 5

// Two DIFFERENT leases protect two different intervals (P5 panel — one
// shared constant cannot serve both):
//
//   - birthLeaseWindow leases a freshly-created delivery row to its
//     in-process first-attempt goroutine: one HTTP attempt (10s client
//     timeout) + endpoint/event reads + the mark, with generous
//     headroom. The retry worker is the crash backstop — it picks the
//     row up at lease expiry, so crash-recovery latency for a money
//     webhook is bounded by this window plus one worker tick.
//   - retryClaimLease covers a whole CLAIMED RETRY BATCH: per-row cost
//     is the 10s HTTP timeout plus three per-row DB round-trips
//     (~13s budgeted), times the batch, plus margin. Derived from the
//     batch size, never hand-tuned — an undersized lease re-claims rows
//     under a live worker and double-POSTs.
//
// A marked outcome always overwrites next_retry_at, so neither lease
// ever delays a legitimately-scheduled retry; they only bound crash
// recovery and cross-worker exclusion.
const (
	birthLeaseWindow  = 120 * time.Second
	perRetryRowBudget = 13 * time.Second
	retryLeaseMargin  = 60 * time.Second
)

func retryClaimLease(batch int) time.Duration {
	return time.Duration(batch)*perRetryRowBudget + retryLeaseMargin
}

// retryBackoffs is the delay before each retry: slot i is the wait after
// the (i+1)-th attempt fails. All maxRetries slots are reachable — the
// final 24h slot fires after the 5th attempt, before the 6th and last.
var retryBackoffs = [maxRetries]time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	24 * time.Hour,
}

type Service struct {
	store       Store
	client      HTTPClient
	bus         *EventBus
	syncDeliver bool // When true, deliver synchronously (for tests)
	// allowLoopback is the development exception to the SSRF posture:
	// loopback endpoint URLs (localhost / 127.x / ::1) are accepted at
	// create AND dialable at delivery. It must be one flag read by BOTH
	// layers — this shipped as two independent decisions once (the
	// validator allowed localhost "for local development" while the
	// egress-hardened dialer refused all loopback unconditionally), and
	// the result was an endpoint the operator could create but Velox
	// could never deliver to: every delivery sat pending with the refusal
	// visible only in the delivery detail. In production the exception is
	// OFF at both layers, because a tenant-supplied loopback URL is SSRF
	// against the deployment itself (velox's own :8080 included).
	allowLoopback bool
}

// HTTPClient is the interface for making HTTP requests (mockable in tests).
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

func NewService(store Store, client HTTPClient) *Service {
	allowLoopback := !strings.EqualFold(strings.TrimSpace(os.Getenv("APP_ENV")), "production")
	if client == nil {
		client = &http.Client{
			Timeout:   10 * time.Second,
			Transport: ssrfHardenedTransport(allowLoopback),
		}
	}
	return &Service{store: store, client: client, bus: NewEventBus(), allowLoopback: allowLoopback}
}

// ssrfHardenedTransport returns an http.Transport whose DialContext rejects
// connections to private/link-local/loopback/metadata IPs at the actual dial
// address. validateWebhookURL only resolves at CreateEndpoint time, so a host
// that resolved publicly then could be re-pointed at an internal address
// (DNS rebinding) by the time delivery POSTs to the stored URL. Checking the
// resolved dial target on every connection closes that window.
func ssrfHardenedTransport(allowLoopback bool) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return ssrfSafeDialContext(ctx, network, addr, allowLoopback)
	}
	return t
}

// ssrfSafeDialContext is the dial predicate wired into the delivery client.
// It resolves the host portion of addr and refuses to dial if the target
// address is a blocked (private/internal) IP.
func ssrfSafeDialContext(ctx context.Context, network, addr string, allowLoopback bool) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if err := assertDialAddrAllowed(host, allowLoopback); err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext(ctx, network, net.JoinHostPort(host, port))
}

// assertDialAddrAllowed resolves host (an IP literal or hostname) and returns
// an error if any resolved address is a blocked private/internal IP. A
// hostname that resolves to multiple addresses is rejected if ANY of them is
// blocked — a partially-poisoned DNS answer must not slip an internal hop
// through. Reuses the same isBlockedIP predicate that the CreateEndpoint
// validation path relies on.
//
// allowLoopback carves out exactly ONE class in development: an address whose
// every resolution is loopback. Private/link-local/metadata ranges stay
// blocked even in dev — the exception exists so a local receiver on
// 127.0.0.1 is reachable, not so a dev deployment can probe its LAN.
func assertDialAddrAllowed(host string, allowLoopback bool) error {
	if ip := net.ParseIP(host); ip != nil {
		if allowLoopback && ip.IsLoopback() {
			return nil
		}
		if isBlockedIP(ip) {
			return fmt.Errorf("refusing to dial private/internal address %s", ip)
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if allowLoopback && ip.IsLoopback() {
			continue
		}
		if isBlockedIP(ip) {
			return fmt.Errorf("refusing to dial private/internal address %s (resolved from %q)", ip, host)
		}
	}
	return nil
}

// isBlockedIP returns true if ip is private, link-local, loopback, or
// otherwise reserved — covering both the IPv4 privateRanges table and the
// IPv6 equivalents (::1, fe80::/10, fc00::/7) that the IPv4-only table misses.
func isBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() || ip.IsUnspecified() {
		return true
	}
	return isPrivateIP(ip)
}

// EventBus exposes the in-memory pub/sub backing the SSE stream. The
// handler subscribes per-request; the service publishes frames from
// Dispatch and the deliver path. Returns the same instance for the
// lifetime of the service so multiple handlers / cmd-side workers
// share one fan-out.
func (s *Service) EventBus() *EventBus { return s.bus }

// publishFrameForEventID looks up the event row and emits a status-
// transition frame to the SSE bus. Used from the failure paths where
// the caller has only the delivery (not the full event) in hand.
// Best-effort: a missed publish doesn't block delivery semantics.
func (s *Service) publishFrameForEventID(ctx context.Context, tenantID, eventID, status string, lastAttemptAt *time.Time) {
	if s.bus == nil || eventID == "" {
		return
	}
	ev, err := s.store.GetEvent(ctx, tenantID, eventID)
	if err != nil {
		return
	}
	s.bus.Publish(tenantID, FrameFromEvent(ev, status, lastAttemptAt))
}

// NewTestService creates a service with synchronous delivery (no goroutines).
func NewTestService(store Store, client HTTPClient) *Service {
	svc := NewService(store, client)
	svc.syncDeliver = true
	return svc
}

// privateRanges defines CIDR blocks that webhook URLs must not resolve to.
var privateRanges = []net.IPNet{
	parseCIDR("10.0.0.0/8"),     // RFC 1918
	parseCIDR("172.16.0.0/12"),  // RFC 1918
	parseCIDR("192.168.0.0/16"), // RFC 1918
	parseCIDR("127.0.0.0/8"),    // Loopback
	parseCIDR("169.254.0.0/16"), // Link-local
	parseCIDR("0.0.0.0/8"),      // "This" network
}

func parseCIDR(s string) net.IPNet {
	_, ipNet, err := net.ParseCIDR(s)
	if err != nil {
		panic("invalid CIDR: " + s)
	}
	return *ipNet
}

// isPrivateIP returns true if the IP falls within a blocked private/reserved range.
func isPrivateIP(ip net.IP) bool {
	for _, r := range privateRanges {
		if r.Contains(ip) {
			return true
		}
	}
	return false
}

// isLoopbackHost reports whether host names the local machine: the literal
// "localhost" or a loopback IP (127.0.0.0/8, ::1). Exact semantics matter —
// the previous check was strings.HasPrefix(host, "localhost"), which waved
// "localhost.evil.com" through the HTTPS requirement as plaintext HTTP to an
// arbitrary public host.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateWebhookURL checks that a webhook URL uses HTTPS and does not
// resolve to a private/internal IP address (SSRF protection). allowLoopback
// (development only) admits plain-http loopback URLs; it is the SAME flag the
// delivery dialer honors, so what create accepts, delivery can reach — and in
// production a loopback URL is refused HERE, loudly, instead of minting an
// endpoint whose every delivery would die in the dialer.
func validateWebhookURL(rawURL string, allowLoopback bool) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return errs.Invalid("url", "must be a valid URL")
	}
	host := parsed.Hostname()
	loopback := isLoopbackHost(host)

	if loopback {
		if !allowLoopback {
			return errs.Invalid("url", "loopback addresses are not deliverable in production — use a publicly reachable HTTPS URL")
		}
		if parsed.Scheme != "https" && parsed.Scheme != "http" {
			return errs.Invalid("url", "must be an http(s) URL")
		}
		return nil
	}
	if parsed.Scheme != "https" {
		return errs.Invalid("url", "must use HTTPS (loopback is exempt in development)")
	}

	// Resolve hostname and check all IPs against blocked ranges.
	ips, err := net.LookupHost(host)
	if err != nil {
		return errs.Invalid("url", fmt.Sprintf("cannot resolve host %q", host))
	}
	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		if isPrivateIP(ip) {
			return errs.Invalid("url", fmt.Sprintf("must not resolve to a private/internal IP address (got %s)", ipStr))
		}
	}

	return nil
}

type CreateEndpointInput struct {
	URL         string   `json:"url"`
	Description string   `json:"description,omitempty"`
	Events      []string `json:"events"`
}

// validateEventNames rejects subscriptions to event types the engine never
// emits. Pre-2026-07-05 any string was accepted and matchesEvent silently
// skipped unknown names — a subscriber to a Stripe-trained name (e.g.
// "invoice.payment_failed"; real name payment.failed) got silence forever,
// and Velox's own recipes shipped two such phantoms. "*" subscribes to
// everything.
func validateEventNames(events []string) error {
	for _, e := range events {
		if e == "*" {
			continue
		}
		// Prefix wildcards ("invoice.*") are a matchesEvent feature — valid
		// iff at least one emitted event carries the prefix.
		if prefix, ok := strings.CutSuffix(e, ".*"); ok {
			matchesAny := false
			for known := range domain.KnownWebhookEventTypes {
				if strings.HasPrefix(known, prefix+".") {
					matchesAny = true
					break
				}
			}
			if matchesAny {
				continue
			}
		}
		if !domain.KnownWebhookEventTypes[e] {
			return errs.Invalid("events", fmt.Sprintf(
				"unknown event type %q — Velox never emits it, so this subscription would receive silence; see the event catalog in docs/webhooks.md", e))
		}
	}
	return nil
}

// UpdateEndpointInput is the PATCH shape: nil = leave unchanged.
type UpdateEndpointInput struct {
	URL         *string   `json:"url,omitempty"`
	Description *string   `json:"description,omitempty"`
	Events      *[]string `json:"events,omitempty"`
	Active      *bool     `json:"active,omitempty"`
}

// UpdateEndpoint mutates url/description/events/active without rotating
// the signing secret (Stripe's update shape). This is also what makes
// recipe-created endpoints usable: they are created inactive with a
// placeholder URL, and this is the "point it at a real URL and activate"
// surface that previously didn't exist.
func (s *Service) UpdateEndpoint(ctx context.Context, tenantID, id string, input UpdateEndpointInput) (domain.WebhookEndpoint, error) {
	ep, err := s.store.GetEndpoint(ctx, tenantID, id)
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	if input.URL != nil {
		rawURL := strings.TrimSpace(*input.URL)
		if rawURL == "" {
			return domain.WebhookEndpoint{}, errs.Required("url")
		}
		if err := validateWebhookURL(rawURL, s.allowLoopback); err != nil {
			return domain.WebhookEndpoint{}, err
		}
		ep.URL = rawURL
	}
	if input.Description != nil {
		ep.Description = strings.TrimSpace(*input.Description)
	}
	if input.Events != nil {
		events := *input.Events
		if len(events) == 0 {
			events = []string{"*"}
		}
		if err := validateEventNames(events); err != nil {
			return domain.WebhookEndpoint{}, err
		}
		ep.Events = events
	}
	if input.Active != nil {
		ep.Active = *input.Active
	}
	return s.store.UpdateEndpoint(ctx, tenantID, ep)
}

type CreateEndpointResult struct {
	Endpoint domain.WebhookEndpoint `json:"endpoint"`
	Secret   string                 `json:"secret"` // Shown once
}

func (s *Service) CreateEndpoint(ctx context.Context, tenantID string, input CreateEndpointInput) (CreateEndpointResult, error) {
	rawURL := strings.TrimSpace(input.URL)
	if rawURL == "" {
		return CreateEndpointResult{}, errs.Required("url")
	}
	if err := validateWebhookURL(rawURL, s.allowLoopback); err != nil {
		return CreateEndpointResult{}, err
	}

	events := input.Events
	if len(events) == 0 {
		events = []string{"*"}
	}
	if err := validateEventNames(events); err != nil {
		return CreateEndpointResult{}, err
	}

	// Generate signing secret. A short read from the entropy pool would yield a
	// low-entropy or partially-zeroed HMAC key that still validates downstream —
	// treat any rand.Read error as fatal to endpoint creation rather than
	// minting a compromised secret.
	secretBytes := make([]byte, 24)
	if _, err := rand.Read(secretBytes); err != nil {
		return CreateEndpointResult{}, fmt.Errorf("generate webhook secret: %w", err)
	}
	secret := "whsec_" + hex.EncodeToString(secretBytes)

	ep, err := s.store.CreateEndpoint(ctx, tenantID, domain.WebhookEndpoint{
		URL:         rawURL,
		Description: strings.TrimSpace(input.Description),
		Secret:      secret,
		Events:      events,
		Active:      true,
	})
	if err != nil {
		return CreateEndpointResult{}, err
	}

	return CreateEndpointResult{Endpoint: ep, Secret: secret}, nil
}

// CreateEndpointTx forwards to the store's tx-aware insert. Used by
// recipe.Service.Instantiate so a recipe with a default outbound endpoint
// commits atomically with the rest of the recipe.
func (s *Service) CreateEndpointTx(ctx context.Context, tx *sql.Tx, tenantID string, ep domain.WebhookEndpoint) (domain.WebhookEndpoint, error) {
	return s.store.CreateEndpointTx(ctx, tx, tenantID, ep)
}

// SecretRotationGracePeriod is how long a rotated-out secret keeps
// signing alongside its replacement. Velox uses 72h; Stripe's hosted
// equivalent caps at 24h
// (https://docs.stripe.com/webhooks/signature). The intentional
// deviation: self-hosted Velox deployments often have slower deploy
// cadences (manual rolls, no central CI fleet), so 24h would force a
// rushed cutover for tenants whose ops team checks the webhook
// receivers once a week. 72h covers a typical "find the change request
// → ship → verify" loop without the rushed-deploy footgun. Compromise
// case: a leaked secret stays usable for up to 72h after the operator
// rotates — bounded but not instant. Tighten to 24h if a tenant-
// configurable cap is justified by a real DP request.
const SecretRotationGracePeriod = 72 * time.Hour

// RotateSecret generates a new signing secret for an endpoint and returns it
// alongside the grace-period expiry. The previous secret stays valid for
// SecretRotationGracePeriod — dispatcher signs outbound events with BOTH
// secrets during the window (two v1= entries in Velox-Signature, Stripe
// multi-signature style) so receivers can stage a verifier update without
// breaking production traffic. After the window, only the new secret is
// used. The new secret is returned once to the caller (dashboard shows it,
// then it's no longer retrievable).
func (s *Service) RotateSecret(ctx context.Context, tenantID, endpointID string) (RotateSecretResult, error) {
	secretBytes := make([]byte, 24)
	if _, err := rand.Read(secretBytes); err != nil {
		return RotateSecretResult{}, fmt.Errorf("generate webhook secret: %w", err)
	}
	newSecret := "whsec_" + hex.EncodeToString(secretBytes)

	ep, err := s.store.RotateEndpointSecret(ctx, tenantID, endpointID, newSecret, SecretRotationGracePeriod)
	if err != nil {
		return RotateSecretResult{}, err
	}
	return RotateSecretResult{
		Secret:             newSecret,
		SecondaryValidTill: ep.SecondarySecretExpiresAt,
	}, nil
}

// RotateSecretResult carries the new secret and the expiry of the
// grace-period sibling. Exposed on the handler response so the dashboard
// can show "old secret valid until <time>" copy.
type RotateSecretResult struct {
	Secret             string     `json:"secret"`
	SecondaryValidTill *time.Time `json:"secondary_valid_until,omitempty"`
}

func (s *Service) ListEndpoints(ctx context.Context, tenantID string) ([]domain.WebhookEndpoint, error) {
	return s.store.ListEndpoints(ctx, tenantID)
}

func (s *Service) DeleteEndpoint(ctx context.Context, tenantID, id string) error {
	return s.store.DeleteEndpoint(ctx, tenantID, id)
}

// Dispatch creates a webhook event and delivers it to all matching endpoints.
//
// Mode scoping: ListEndpoints runs under the caller's ctx livemode, which
// RLS already filters on. The explicit ep.Livemode == event.Livemode check
// below is defense-in-depth — if a future call path opens a bypass tx or
// the RLS predicate is relaxed, test-mode events must still never cross
// into a live endpoint (and vice versa). Cross-mode delivery would leak
// synthetic data into production monitoring.
func (s *Service) Dispatch(ctx context.Context, tenantID, eventType string, payload map[string]any) error {
	return s.DispatchFromOutbox(ctx, "", tenantID, eventType, payload)
}

// DispatchFromOutbox is Dispatch carrying the webhook_outbox row that
// produced this event, so the row's dispatched-mark commits ATOMICALLY
// with the event + delivery rows (handler-owns-mark, P5): the old
// separate mark meant a crash between the event commit and the outbox
// mark re-ran the handler next tick and minted a duplicate event with a
// fresh id — receivers cannot dedupe those. Empty outboxRowID = direct
// dispatch (no outbox row).
//
// The endpoint fan-out is resolved FIRST (the event's livemode is the
// ctx's — the 0021 trigger stamps the row from the same session GUC)
// and the event + every matching delivery row are created in ONE store
// tx, rows born leased (birthLeaseWindow): the in-process goroutines
// own the first attempt; a crash any time after the commit leaves rows
// the retry worker picks up at lease expiry — never an event no
// delivery row references, never a NULL next_retry_at.
func (s *Service) DispatchFromOutbox(ctx context.Context, outboxRowID, tenantID, eventType string, payload map[string]any) error {
	endpoints, err := s.store.ListEndpoints(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("list endpoints: %w", err)
	}
	evLivemode := postgres.Livemode(ctx)
	var matched []domain.WebhookEndpoint
	for _, ep := range endpoints {
		if !ep.Active || ep.Livemode != evLivemode || !matchesEvent(ep.Events, eventType) {
			continue
		}
		matched = append(matched, ep)
	}
	epIDs := make([]string, len(matched))
	for i, ep := range matched {
		epIDs[i] = ep.ID
	}

	event, deliveries, err := s.store.CreateEventWithDeliveries(ctx, tenantID, domain.WebhookEvent{
		EventType: eventType,
		Payload:   payload,
	}, epIDs, birthLeaseWindow, outboxRowID)
	if err != nil {
		return fmt.Errorf("create event: %w", err)
	}

	// Publish a "pending" frame the moment the event commits — the
	// dashboard renders it immediately, then flips when the attempt
	// goroutine publishes the outcome frame.
	if s.bus != nil {
		s.bus.Publish(tenantID, FrameFromEvent(event, "pending", nil))
	}

	s.spawnAttempts(ctx, event, matched, deliveries)
	return nil
}

// spawnAttempts fires the first-attempt goroutines for freshly created
// (born-leased) delivery rows. Goroutines run on a fresh ctx pinned to
// the event's livemode (the producing request may return first); the
// sync path (tests) re-pins the caller ctx for the same reason.
func (s *Service) spawnAttempts(ctx context.Context, event domain.WebhookEvent, matched []domain.WebhookEndpoint, deliveries []domain.WebhookDelivery) {
	for i := range matched {
		ep, d := matched[i], deliveries[i]
		if s.syncDeliver {
			s.attemptDelivery(postgres.WithLivemode(ctx, event.Livemode), d, ep, event)
			continue
		}
		dCtx := postgres.WithLivemode(context.Background(), event.Livemode)
		go s.attemptDelivery(dCtx, d, ep, event)
	}
}

// scheduleRetryOrFail increments the attempt count and either schedules a retry
// with exponential backoff or marks the delivery as permanently failed.
func (s *Service) scheduleRetryOrFail(ctx context.Context, tenantID string, d domain.WebhookDelivery, errMsg string) {
	d.AttemptCount++
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}
	d.ErrorMessage = errMsg

	// Permanent fail once every retry slot is spent: after the
	// AttemptCount-th failure the next retry would read
	// retryBackoffs[AttemptCount-1], so when AttemptCount exceeds
	// maxRetries the ladder is exhausted (nothing left to wait on).
	// Using `>` here — not `>=` — is what makes the final 24h slot
	// reachable: AttemptCount==maxRetries still schedules the last retry.
	if d.AttemptCount > maxRetries {
		now := time.Now().UTC()
		d.Status = domain.DeliveryFailed
		d.CompletedAt = &now
		d.NextRetryAt = nil
		// Only count permanent failures — per-attempt retries are transient
		// and would drown the alert's success-rate denominator.
		mw.RecordWebhookDelivery("failed")
		// SSE: the dashboard row for this event flips to "failed".
		// Lookup-by-id rather than a struct passthrough so we re-read
		// the event_type/customer_id without bloating the delivery row.
		s.publishFrameForEventID(ctx, tenantID, d.WebhookEventID, "failed", &now)
		slog.Error("webhook delivery permanently failed",
			"delivery_id", d.ID,
			"endpoint_id", d.WebhookEndpointID,
			"attempts", d.AttemptCount,
			"error", errMsg,
		)
	} else {
		jitter := time.Duration(mrand.IntN(30)) * time.Second
		nextRetry := time.Now().UTC().Add(retryBackoffs[d.AttemptCount-1] + jitter)
		d.Status = domain.DeliveryPending
		d.NextRetryAt = &nextRetry
		slog.Warn("webhook delivery scheduled for retry",
			"delivery_id", d.ID,
			"endpoint_id", d.WebhookEndpointID,
			"attempt", d.AttemptCount,
			"next_retry_at", nextRetry.Format(time.RFC3339),
			"error", errMsg,
		)
	}

	if _, err := s.store.UpdateDelivery(ctx, tenantID, d); err != nil {
		slog.Warn("webhook: retry/fail mark not applied", "delivery_id", d.ID, "error", err)
	}
}

func matchesEvent(subscribed []string, eventType string) bool {
	for _, s := range subscribed {
		if s == "*" || s == eventType {
			return true
		}
		// Prefix match: "invoice.*" matches "invoice.created"
		if strings.HasSuffix(s, ".*") {
			prefix := strings.TrimSuffix(s, ".*")
			if strings.HasPrefix(eventType, prefix+".") {
				return true
			}
		}
	}
	return false
}

func computeSignature(payload []byte, timestamp, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "." + string(payload)))
	return hex.EncodeToString(mac.Sum(nil))
}

// buildSignatureHeader produces the Velox-Signature header value.
//
// In steady state, it's a single-sig "t=<ts>,v1=<sig>". During a
// grace-period rotation (secondary secret populated and not yet
// expired), it emits two v1= entries — the same multi-signature format
// Stripe uses on Stripe-Signature. Receivers that follow the
// "verify ANY v1= matches" convention pass through the rotation without
// a production outage while their code deploys the new verifier.
//
// `now` is taken as a parameter (rather than called internally) so unit
// tests can exercise pre- and post-expiry branches deterministically.
func buildSignatureHeader(timestamp string, body []byte, ep domain.WebhookEndpoint, now time.Time) string {
	primary := computeSignature(body, timestamp, ep.Secret)
	if ep.SecondarySecret == "" || ep.SecondarySecretExpiresAt == nil || !now.Before(*ep.SecondarySecretExpiresAt) {
		return fmt.Sprintf("t=%s,v1=%s", timestamp, primary)
	}
	secondary := computeSignature(body, timestamp, ep.SecondarySecret)
	return fmt.Sprintf("t=%s,v1=%s,v1=%s", timestamp, primary, secondary)
}

// GetEndpointStats returns delivery success/failure stats per endpoint.
func (s *Service) GetEndpointStats(ctx context.Context, tenantID string) ([]EndpointStats, error) {
	return s.store.GetEndpointStats(ctx, tenantID)
}

// ListEvents returns recent webhook events for a tenant.
func (s *Service) ListEvents(ctx context.Context, tenantID string, limit int) ([]domain.WebhookEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.store.ListEvents(ctx, tenantID, limit)
}

// EventDeliveryStatuses rolls each event's deliveries up to one dashboard
// status — see the Store method for the precedence contract.
func (s *Service) EventDeliveryStatuses(ctx context.Context, tenantID string, eventIDs []string) (map[string]string, error) {
	return s.store.EventDeliveryStatuses(ctx, tenantID, eventIDs)
}

// GetEvent fetches a single event by id (tenant-scoped via RLS at the
// store layer). Surfaced for the SSE handler's deliveries-list path so
// it can resolve the replay root before walking the timeline.
func (s *Service) GetEvent(ctx context.Context, tenantID, id string) (domain.WebhookEvent, error) {
	return s.store.GetEvent(ctx, tenantID, id)
}

// ListDeliveries returns deliveries for a specific event.
func (s *Service) ListDeliveries(ctx context.Context, tenantID, eventID string) ([]domain.WebhookDelivery, error) {
	return s.store.ListDeliveries(ctx, tenantID, eventID)
}

// ReplayResult is the response shape for POST /v1/webhook_events/{id}/replay.
// Returned to the dashboard so the live tail can highlight the new row
// and the Toast confirms what the operator just queued.
type ReplayResult struct {
	// EventID is the freshly-minted webhook_events row that's been
	// queued for delivery. The dashboard's SSE tail will pick it up
	// within a tick.
	EventID string `json:"event_id"`
	// ReplayOf is the ID of the original event whose payload was
	// cloned. The dashboard groups the original + all replays under
	// this pivot so the Deliveries timeline shows the full audit
	// chain.
	ReplayOf string `json:"replay_of"`
	// Status is "queued" — the deliver fan-out runs asynchronously, so
	// at response time we can only confirm the clone landed and
	// matched at least one endpoint.
	Status string `json:"status"`
}

// Replay clones an existing webhook event into a fresh row (with
// replay_of_event_id pointing at the original) and dispatches it to
// every matching active endpoint. The clone is what gets delivered, so
// the original's deliveries are never mutated — every replay produces
// a brand-new row in the timeline. A second replay of the same
// original event is therefore not idempotent in the DB-row sense (it
// creates another clone), but is idempotent in the audit-trail sense:
// the original delivery history is preserved and the operator sees N
// distinct replay attempts on the timeline.
func (s *Service) Replay(ctx context.Context, tenantID, eventID string) (ReplayResult, error) {
	original, err := s.store.GetEvent(ctx, tenantID, eventID)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return ReplayResult{}, fmt.Errorf("%w: webhook event", errs.ErrNotFound)
		}
		return ReplayResult{}, fmt.Errorf("get event: %w", err)
	}

	clone, err := s.store.CreateReplayEvent(ctx, tenantID, eventID)
	if err != nil {
		return ReplayResult{}, fmt.Errorf("create replay event: %w", err)
	}

	// Pivot ID: the audit chain points at the *root* original. If the
	// operator clicked Replay on a clone, the store's CreateReplayEvent
	// already collapsed that to root — surface that root in the
	// response so the dashboard groups consistently.
	rootID := eventID
	if clone.ReplayOfEventID != nil && *clone.ReplayOfEventID != "" {
		rootID = *clone.ReplayOfEventID
	}

	// Publish the immediate "pending" frame for the clone so the
	// dashboard shows the new row before the deliver fan-out lands.
	if s.bus != nil {
		s.bus.Publish(tenantID, FrameFromEvent(clone, "pending", nil))
	}

	endpoints, err := s.store.ListEndpoints(ctx, tenantID)
	if err != nil {
		return ReplayResult{}, fmt.Errorf("list endpoints: %w", err)
	}
	var matched []domain.WebhookEndpoint
	for _, ep := range endpoints {
		if !ep.Active || ep.Livemode != clone.Livemode || !matchesEvent(ep.Events, clone.EventType) {
			continue
		}
		matched = append(matched, ep)
	}
	epIDs := make([]string, len(matched))
	for i, ep := range matched {
		epIDs[i] = ep.ID
	}
	// Replay rows are born leased in one batch tx like Dispatch's (the
	// clone event itself committed in CreateReplayEvent — an operator
	// replay interrupted between the two just gets clicked again).
	deliveries, err := s.store.CreateDeliveriesForEvent(postgres.WithLivemode(ctx, clone.Livemode), tenantID, clone.ID, epIDs, birthLeaseWindow)
	if err != nil {
		return ReplayResult{}, fmt.Errorf("create replay deliveries: %w", err)
	}
	s.spawnAttempts(ctx, clone, matched, deliveries)

	slog.Info("webhook event replayed",
		"event_id", clone.ID,
		"replay_of", rootID,
		"event_type", clone.EventType,
	)
	_ = original // referenced for not-found semantics above
	status := "queued"
	if len(matched) == 0 {
		status = "no_matching_endpoints"
	}
	return ReplayResult{
		EventID:  clone.ID,
		ReplayOf: rootID,
		Status:   status,
	}, nil
}

// ListEndpointDeliveries returns one endpoint plus its delivery history
// (newest first). The endpoint lookup runs first so a bad id 404s rather
// than returning an empty list that reads as "healthy receiver, no
// traffic".
func (s *Service) ListEndpointDeliveries(ctx context.Context, tenantID, endpointID string, limit int) (domain.WebhookEndpoint, []domain.WebhookDelivery, error) {
	ep, err := s.store.GetEndpoint(ctx, tenantID, endpointID)
	if err != nil {
		return domain.WebhookEndpoint{}, nil, err
	}
	deliveries, err := s.store.ListDeliveriesByEndpoint(ctx, tenantID, endpointID, limit)
	if err != nil {
		return domain.WebhookEndpoint{}, nil, err
	}
	return ep, deliveries, nil
}

// ReplayDeliveryResult is the response for a single-receiver replay.
type ReplayDeliveryResult struct {
	// EventID is the freshly-minted replay clone (replay_of set), same
	// shape as an event-wide replay — the delivery timeline folds it
	// into the original's chain.
	EventID string `json:"event_id"`
	// ReplayOf is the root original event of the audit chain.
	ReplayOf string `json:"replay_of"`
	// DeliveryID is the new delivery row created for the target
	// endpoint — the drill-down highlights it on refetch.
	DeliveryID string `json:"delivery_id"`
	Status     string `json:"status"`
}

// ReplayDelivery re-sends one delivery's event to THAT delivery's
// endpoint only — the industry replay unit (Stripe "Resend"/"Retry now",
// GitHub "Redeliver", Svix per-attempt resend). Event-wide Replay fans
// the clone out to every matching endpoint, which re-delivers to
// receivers that already succeeded; this path exists so fixing ONE
// broken receiver doesn't spray duplicates at the healthy ones.
//
// Same audit chain as Replay: a clone event (replay_of = root original)
// plus a born-leased delivery row — the original delivery is history and
// is never mutated.
func (s *Service) ReplayDelivery(ctx context.Context, tenantID, endpointID, deliveryID string) (ReplayDeliveryResult, error) {
	d, err := s.store.GetDelivery(ctx, tenantID, deliveryID)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return ReplayDeliveryResult{}, fmt.Errorf("%w: webhook delivery", errs.ErrNotFound)
		}
		return ReplayDeliveryResult{}, fmt.Errorf("get delivery: %w", err)
	}
	// The route carries both ids; a delivery reached under another
	// endpoint's path is treated as absent, not as a hint.
	if d.WebhookEndpointID != endpointID {
		return ReplayDeliveryResult{}, fmt.Errorf("%w: webhook delivery", errs.ErrNotFound)
	}

	ep, err := s.store.GetEndpoint(ctx, tenantID, endpointID)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return ReplayDeliveryResult{}, fmt.Errorf("%w: webhook endpoint", errs.ErrNotFound)
		}
		return ReplayDeliveryResult{}, fmt.Errorf("get endpoint: %w", err)
	}
	// Guards run BEFORE the clone is minted: a rejected replay must leave
	// no event row behind (an orphan clone with zero deliveries would
	// surface as a phantom "no_endpoints" row in the live tail).
	if !ep.Active {
		return ReplayDeliveryResult{}, errs.InvalidState("endpoint is paused — resume it before replaying deliveries")
	}
	// No livemode guard here, deliberately: the tenant_isolation policy
	// scopes BOTH reads above by (tenant, livemode), so a cross-mode
	// delivery/endpoint pair is invisible together — one of the two
	// lookups already 404'd. The 404 IS the cross-partition failure.

	clone, err := s.store.CreateReplayEvent(ctx, tenantID, d.WebhookEventID)
	if err != nil {
		return ReplayDeliveryResult{}, fmt.Errorf("create replay event: %w", err)
	}
	rootID := d.WebhookEventID
	if clone.ReplayOfEventID != nil && *clone.ReplayOfEventID != "" {
		rootID = *clone.ReplayOfEventID
	}

	if s.bus != nil {
		s.bus.Publish(tenantID, FrameFromEvent(clone, "pending", nil))
	}

	deliveries, err := s.store.CreateDeliveriesForEvent(postgres.WithLivemode(ctx, clone.Livemode), tenantID, clone.ID, []string{ep.ID}, birthLeaseWindow)
	if err != nil {
		return ReplayDeliveryResult{}, fmt.Errorf("create replay delivery: %w", err)
	}
	s.spawnAttempts(ctx, clone, []domain.WebhookEndpoint{ep}, deliveries)

	slog.Info("webhook delivery replayed",
		"delivery_id", d.ID,
		"new_delivery_id", deliveries[0].ID,
		"endpoint_id", ep.ID,
		"event_id", clone.ID,
		"replay_of", rootID,
	)
	return ReplayDeliveryResult{
		EventID:    clone.ID,
		ReplayOf:   rootID,
		DeliveryID: deliveries[0].ID,
		Status:     "queued",
	}, nil
}

// RetryPendingDeliveries picks up deliveries due for retry and re-attempts them.
func (s *Service) RetryPendingDeliveries(ctx context.Context) error {
	deliveries, err := s.store.ListPendingDeliveries(ctx, 10)
	if err != nil {
		return fmt.Errorf("list pending deliveries: %w", err)
	}

	if len(deliveries) == 0 {
		return nil
	}

	slog.Info("retrying webhook deliveries", "count", len(deliveries))

	for _, d := range deliveries {
		// ListPendingDeliveries runs in TxBypass (cross-tenant), so we tag
		// the per-delivery ctx with the row's livemode. Every downstream
		// store call opens its own TxTenant and needs this to route the
		// delivery back to the same mode partition. The per-row budget is
		// ENFORCED, not estimated (verifier catch): the retryClaimLease
		// arithmetic assumes ≤perRetryRowBudget per row, and the DB reads
		// here were otherwise unbounded — a degraded DB would break the
		// lease math and double-POST under a live worker.
		rowCtx, rowCancel := context.WithTimeout(ctx, perRetryRowBudget)
		dCtx := postgres.WithLivemode(rowCtx, d.Livemode)

		ep, err := s.store.GetEndpoint(dCtx, d.TenantID, d.WebhookEndpointID)
		if err != nil {
			if errors.Is(err, errs.ErrNotFound) {
				// Endpoint soft-deleted (GetEndpoint filters deleted_at):
				// the delivery can never succeed, and the !Active resolver
				// below is unreachable for it — without a terminal mark
				// here the row is re-claimed at every lease expiry forever,
				// occupying claim slots and spamming this log. Mirror of
				// the event-missing resolver just below (the skip branch
				// must also end the row's wait).
				now := time.Now().UTC()
				d.Status = domain.DeliveryFailed
				d.ErrorMessage = "endpoint deleted"
				d.CompletedAt = &now
				d.NextRetryAt = nil
				if _, err := s.store.UpdateDelivery(dCtx, d.TenantID, d); err != nil {
					slog.Warn("webhook: endpoint-deleted mark not applied", "delivery_id", d.ID, "error", err)
				}
				slog.Error("endpoint not found for retry — delivery resolved failed", "delivery_id", d.ID, "endpoint_id", d.WebhookEndpointID)
				rowCancel()
				continue
			}
			slog.Error("get endpoint for retry", "delivery_id", d.ID, "error", err)
			rowCancel()
			continue
		}
		if !ep.Active {
			// Endpoint was disabled; mark delivery as failed.
			now := time.Now().UTC()
			d.Status = domain.DeliveryFailed
			d.ErrorMessage = "endpoint disabled"
			d.CompletedAt = &now
			d.NextRetryAt = nil
			if _, err := s.store.UpdateDelivery(dCtx, d.TenantID, d); err != nil {
				slog.Warn("webhook: endpoint-disabled mark not applied", "delivery_id", d.ID, "error", err)
			}
			rowCancel()
			continue
		}

		// Point-lookup the event directly. The previous ListEvents+scan
		// clamped to the newest ~100 rows, so older pending deliveries
		// (whose event had aged out of that window) were stranded forever.
		event, err := s.store.GetEvent(dCtx, d.TenantID, d.WebhookEventID)
		if err != nil {
			if errors.Is(err, errs.ErrNotFound) {
				// Event genuinely deleted — the delivery can never succeed.
				// Mark it failed so it stops cycling through the retry pool.
				now := time.Now().UTC()
				d.Status = domain.DeliveryFailed
				d.ErrorMessage = "event not found"
				d.CompletedAt = &now
				d.NextRetryAt = nil
				if _, err := s.store.UpdateDelivery(dCtx, d.TenantID, d); err != nil {
					slog.Warn("webhook: event-missing mark not applied", "delivery_id", d.ID, "error", err)
				}
				slog.Error("event not found for retry", "delivery_id", d.ID, "event_id", d.WebhookEventID)
				rowCancel()
				continue
			}
			slog.Error("get event for retry", "delivery_id", d.ID, "event_id", d.WebhookEventID, "error", err)
			rowCancel()
			continue
		}

		s.attemptDelivery(dCtx, d, ep, event)
		rowCancel()
	}

	return nil
}

// attemptDelivery POSTs one existing delivery row (first attempt from
// the Dispatch fan-out, or a claimed retry) and CAS-marks the outcome.
// Publishes the outcome frame to the SSE bus so the dashboard's live
// tail flips pending→succeeded/failed on BOTH paths (the retry path
// previously never updated the tail).
func (s *Service) attemptDelivery(ctx context.Context, d domain.WebhookDelivery, ep domain.WebhookEndpoint, event domain.WebhookEvent) {
	body := map[string]any{
		"id":         event.ID,
		"event_type": event.EventType,
		"created_at": event.CreatedAt.Format(time.RFC3339),
		"data":       event.Payload,
	}
	bodyBytes, _ := json.Marshal(body)

	now := time.Now().UTC()
	timestamp := fmt.Sprintf("%d", now.Unix())
	sigHeader := buildSignatureHeader(timestamp, bodyBytes, ep, now)

	req, err := http.NewRequestWithContext(ctx, "POST", ep.URL, bytes.NewReader(bodyBytes))
	if err != nil {
		s.scheduleRetryOrFail(ctx, d.TenantID, d, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Velox-Signature", sigHeader)
	req.Header.Set("Velox-Event-Type", event.EventType)

	resp, err := s.client.Do(req)
	if err != nil {
		s.scheduleRetryOrFail(ctx, d.TenantID, d, err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))

	d.HTTPStatusCode = resp.StatusCode
	d.ResponseBody = string(respBody)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		now := time.Now().UTC()
		d.AttemptCount++
		d.Status = domain.DeliverySucceeded
		d.CompletedAt = &now
		d.NextRetryAt = nil
		if _, err := s.store.UpdateDelivery(ctx, d.TenantID, d); err != nil {
			// A stale mark (lease expired, a sibling already resolved the
			// row) is dropped by the CAS — logged, never applied. Any
			// other error is a real mark loss: the POST landed but the
			// row stays pending, so the receiver will see one duplicate
			// after the lease. Surfaced, not swallowed (P5).
			slog.Warn("webhook: success mark not applied", "delivery_id", d.ID, "error", err)
			return
		}
		mw.RecordWebhookDelivery("succeeded")
		if s.bus != nil {
			s.bus.Publish(d.TenantID, FrameFromEvent(event, string(domain.DeliverySucceeded), &now))
		}
		slog.Info("webhook delivered",
			"delivery_id", d.ID,
			"endpoint_id", ep.ID,
			"event_type", event.EventType,
			"attempt", d.AttemptCount,
			"http_status", resp.StatusCode,
		)
	} else {
		s.scheduleRetryOrFail(ctx, d.TenantID, d, fmt.Sprintf("HTTP %d", resp.StatusCode))
	}
}

// StartRetryWorker runs a background loop that retries pending deliveries on
// the given interval. It blocks until the context is cancelled. When the
// store exposes an advisory lock (production Postgres), each tick is
// leader-gated — the claim lease alone is a correct multi-replica guard,
// but gating makes its sizing non-critical (P5; same posture as both
// outbox dispatchers).
func (s *Service) StartRetryWorker(ctx context.Context, interval time.Duration) {
	slog.Info("webhook retry worker started", "interval", interval.String())
	locker, _ := s.store.(RetryLocker)
	scheduler.Run(ctx, "webhook_retry", interval, func(ctx context.Context) {
		if locker != nil {
			lock, acquired, err := locker.TryRetryLock(ctx)
			if err != nil {
				slog.Error("webhook retry worker: lock acquire failed", "error", err)
				return
			}
			if !acquired {
				return
			}
			defer lock.Release()
		}
		if err := s.RetryPendingDeliveries(ctx); err != nil {
			slog.Error("webhook retry worker error", "error", err)
		}
	})
}

// RetryLocker is the optional leader-gate the retry worker uses when the
// store provides it (PostgresStore does; unit-test fakes usually don't).
type RetryLocker interface {
	TryRetryLock(ctx context.Context) (DispatchLock, bool, error)
}
