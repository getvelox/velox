package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

const (
	keyPrefixLen = 12 // hex chars used for DB prefix lookup
	keySecretLen = 32 // random bytes = 64 hex chars
)

type Service struct {
	store Store

	// lastTouched debounces the last_used_at write per key: unix-nano of the
	// last touch this process issued, keyed by api key id. See touchLastUsed.
	lastTouched sync.Map // map[string]*atomic.Int64
	// touchInterval is how often, per key, last_used_at is written. A field
	// so tests can shorten it; production uses lastUsedTouchInterval.
	touchInterval time.Duration
	// now is the clock, injectable for the debounce tests.
	now func() time.Time
}

// lastUsedTouchInterval: last_used_at is written at most once per key per this
// interval. The column feeds "Last used 3 minutes ago" on the API-keys page
// and nothing else — no auth decision reads it — so minute granularity is fine.
//
// Why it is not written on every request: it used to be, from a fire-and-forget
// goroutine per request, and on the AWS benchmark rig that was THE ceiling for
// a single API key. Every request updated the SAME row; a row lock hands off at
// about one round trip (~1.75 ms same-AZ), so one key could not exceed ~570
// requests/s on any hardware, more connections only added waiters, and
// pg_stat_activity sampled mid-run showed 50-55 of 61 backends waiting on
// `UPDATE api_keys SET last_used_at`. Issue #818.
const lastUsedTouchInterval = time.Minute

func NewService(store Store) *Service {
	return &Service{store: store, touchInterval: lastUsedTouchInterval, now: func() time.Time { return time.Now().UTC() }}
}

// touchLastUsed writes last_used_at for key id at most once per touchInterval
// per process, and returns whether it issued the write. Exactly one caller wins
// a given interval even under a concurrent burst: the per-key timestamp is
// advanced with a compare-and-swap, so N simultaneous requests produce one
// UPDATE, not N goroutines queueing on one row.
func (s *Service) touchLastUsed(id string) bool {
	nowNs := s.now().UnixNano()
	v, _ := s.lastTouched.LoadOrStore(id, new(atomic.Int64))
	last := v.(*atomic.Int64)
	prev := last.Load()
	if prev != 0 && nowNs-prev < int64(s.touchInterval) {
		return false
	}
	if !last.CompareAndSwap(prev, nowNs) {
		return false // another request in this instant won the interval
	}
	go func() { _ = s.store.TouchLastUsed(context.Background(), id, time.Unix(0, nowNs).UTC()) }()
	return true
}

// CreateKeyResult contains the key record + raw key (shown once).
type CreateKeyResult struct {
	Key    domain.APIKey `json:"key"`
	RawKey string        `json:"raw_key"`
}

type CreateKeyInput struct {
	Name      string     `json:"name"`
	KeyType   KeyType    `json:"key_type"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func (s *Service) CreateKey(ctx context.Context, tenantID string, input CreateKeyInput) (CreateKeyResult, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return CreateKeyResult{}, errs.Required("name")
	}

	keyType := input.KeyType
	if keyType == "" {
		keyType = KeyTypeSecret
	}
	if keyType != KeyTypePlatform && keyType != KeyTypeSecret && keyType != KeyTypePublishable {
		return CreateKeyResult{}, errs.Invalid("key_type", "must be one of: platform, secret, publishable")
	}

	// Platform keys are a privileged credential: they carry tenant-management
	// permissions (PermTenantRead/Write) that reach ACROSS tenants, and the
	// `tenants` surface they unlock is not RLS-protected. `key_type` arrives
	// from the request body, and the /v1/api-keys route is reachable by any
	// secret key or dashboard session (both hold PermAPIKeyWrite, both scoped
	// to a single tenant). Without this gate a single-tenant principal could
	// self-mint a platform key and escalate to read/create every tenant.
	// Treat key_type as privileged: a platform key may only be issued by an
	// existing platform principal. (The first platform key, if a deployment
	// ever needs one, is provisioned out-of-band, not over this route.)
	if keyType == KeyTypePlatform && GetKeyType(ctx) != KeyTypePlatform {
		return CreateKeyResult{}, errs.Forbidden(
			"platform keys can only be issued by an existing platform key")
	}

	// Reject past expires_at — a key that's already expired at creation
	// is DOA (any auth attempt fails with `api key expired`). Industry
	// parity (Stripe rejects past expiry on credentials). API keys are
	// tenant-level resources used outside any per-customer clock-pin
	// context, so wall-clock comparison is the correct reference frame.
	if input.ExpiresAt != nil && !input.ExpiresAt.After(time.Now().UTC()) {
		return CreateKeyResult{}, errs.Invalid("expires_at",
			"must be in the future — a key that expires at or before now is dead on arrival")
	}

	// Mode inherits from caller ctx, matching Stripe: a test-mode key creates
	// new test-mode keys, a live-mode key creates live. There is no cross-mode
	// key creation path — caller must switch authenticators to switch mode.
	livemode := Livemode(ctx)

	// Generate raw key: prefix + random hex
	secret := make([]byte, keySecretLen)
	if _, err := rand.Read(secret); err != nil {
		return CreateKeyResult{}, fmt.Errorf("generate key: %w", err)
	}
	secretHex := hex.EncodeToString(secret)
	prefix := KeyPrefix(keyType, livemode)
	rawKey := prefix + secretHex

	// DB prefix: full mode-aware prefix + first N hex chars (indexed lookup)
	dbPrefix := prefix + secretHex[:keyPrefixLen]

	// Generate a 16-byte random salt for this key
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return CreateKeyResult{}, fmt.Errorf("generate salt: %w", err)
	}
	saltHex := hex.EncodeToString(salt)

	// Hash: SHA-256(salt + rawKey)
	hash := sha256.Sum256(append(salt, []byte(rawKey)...))
	hashHex := hex.EncodeToString(hash[:])

	id := postgres.NewID("vlx_key")

	key, err := s.store.Create(ctx, domain.APIKey{
		ID:        id,
		KeyPrefix: dbPrefix,
		KeyHash:   hashHex,
		KeySalt:   saltHex,
		KeyType:   string(keyType),
		Livemode:  livemode,
		Name:      name,
		TenantID:  tenantID,
		ExpiresAt: input.ExpiresAt,
	})
	if err != nil {
		return CreateKeyResult{}, err
	}

	return CreateKeyResult{Key: key, RawKey: rawKey}, nil
}

// ErrInvalidKey marks a DEFINITIVE credential rejection — bad format, no
// matching key, hash mismatch, or expiry. Middlewares map it (and only it)
// to the generic 401. Any other ValidateKey error means the verdict never
// happened (store/DB failure) and must surface as a retryable 5xx: during a
// DB outage a 401 tells every integrator their key went bad and to start
// rotating credentials, which is a lie about the key (#560). A fleet-wide
// 503 reveals nothing about any individual key, so ADR-026's
// anti-enumeration stance is preserved.
var ErrInvalidKey = errors.New("invalid api key")

// ValidateKey looks up a key by prefix, verifies hash, checks expiry.
//
// Accepts three prefix forms, in priority order:
//  1. "vlx_{type}_live_..." (new, post-FEAT-8)
//  2. "vlx_{type}_test_..." (new, post-FEAT-8)
//  3. "vlx_{type}_..."       (legacy, pre-FEAT-8 — treated as live)
//
// Backward compat for (3) keeps existing production keys working through
// the cutover. Newly issued keys always use form (1) or (2).
func (s *Service) ValidateKey(ctx context.Context, rawKey string) (domain.APIKey, error) {
	rawKey = strings.TrimSpace(rawKey)

	var (
		keyType    KeyType
		fullPrefix string
	)
	for _, kt := range []KeyType{KeyTypePlatform, KeyTypeSecret, KeyTypePublishable} {
		typeP := kt.TypePrefix()
		if !strings.HasPrefix(rawKey, typeP) {
			continue
		}
		after := rawKey[len(typeP):]
		switch {
		case strings.HasPrefix(after, "live_"):
			keyType = kt
			fullPrefix = typeP + "live_"
		case strings.HasPrefix(after, "test_"):
			keyType = kt
			fullPrefix = typeP + "test_"
		default:
			// Legacy format (pre-FEAT-8): no mode infix — treat as live.
			keyType = kt
			fullPrefix = typeP
		}
		break
	}
	if fullPrefix == "" {
		return domain.APIKey{}, fmt.Errorf("invalid key format: %w", ErrInvalidKey)
	}
	_ = keyType // retained for future use (permission routing pre-lookup)

	secretPart := strings.TrimPrefix(rawKey, fullPrefix)
	if len(secretPart) < keyPrefixLen {
		return domain.APIKey{}, fmt.Errorf("invalid key format: %w", ErrInvalidKey)
	}

	dbPrefix := fullPrefix + secretPart[:keyPrefixLen]

	key, err := s.store.GetByPrefix(ctx, dbPrefix)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return domain.APIKey{}, ErrInvalidKey
		}
		// The lookup never reached a verdict — this is an infrastructure
		// failure, not a credential one. Callers surface it as 5xx.
		return domain.APIKey{}, fmt.Errorf("api key lookup: %w", err)
	}

	// Verify hash using the stored salt
	salt, err := hex.DecodeString(key.KeySalt)
	if err != nil {
		return domain.APIKey{}, fmt.Errorf("undecodable key salt: %w", ErrInvalidKey)
	}
	hash := sha256.Sum256(append(salt, []byte(rawKey)...))
	hashHex := hex.EncodeToString(hash[:])
	if subtle.ConstantTimeCompare([]byte(hashHex), []byte(key.KeyHash)) != 1 {
		return domain.APIKey{}, ErrInvalidKey
	}

	// Check expiration
	if key.ExpiresAt != nil && time.Now().UTC().After(*key.ExpiresAt) {
		return domain.APIKey{}, fmt.Errorf("api key expired: %w", ErrInvalidKey)
	}

	// Touch last used — debounced, at most once per key per interval (#818).
	s.touchLastUsed(key.ID)

	return key, nil
}

func (s *Service) RevokeKey(ctx context.Context, tenantID, id string) (domain.APIKey, error) {
	return s.store.Revoke(ctx, tenantID, id)
}

// MaxRotationGraceSeconds caps how long a caller can keep the old key alive
// during rotation. Seven days matches the longest grace window Stripe offers
// in its dashboard "roll key" flow — long enough for a scheduled deploy
// cadence, short enough that a forgotten rotation still self-heals.
const MaxRotationGraceSeconds = 7 * 24 * 60 * 60

// RotateKeyInput parameterises rotation. ExpiresInSeconds=0 revokes the old
// key immediately on rotation; a positive value keeps it valid for that many
// seconds past now, giving deployed clients time to swap credentials without
// a hard cutover.
type RotateKeyInput struct {
	ExpiresInSeconds int64 `json:"expires_in_seconds,omitempty"`
}

// RotateKeyResult returns both sides of a rotation. RawKey is the new key's
// plaintext — shown once, never persisted or returned on any subsequent
// fetch. OldKey's ExpiresAt (grace window) or RevokedAt (immediate) reflects
// which path the caller took.
type RotateKeyResult struct {
	OldKey domain.APIKey `json:"old_key"`
	NewKey domain.APIKey `json:"new_key"`
	RawKey string        `json:"raw_key"`
}

// RotateKey issues a replacement key for `id`, preserving its name, type,
// livemode, and any configured expires_at. The old key is either revoked
// immediately (grace=0) or scheduled to expire after input.ExpiresInSeconds
// seconds; callers trading zero-downtime deploys for slightly wider blast
// radius pick a non-zero grace.
func (s *Service) RotateKey(ctx context.Context, tenantID, id string, input RotateKeyInput) (RotateKeyResult, error) {
	if input.ExpiresInSeconds < 0 {
		return RotateKeyResult{}, errs.Invalid("expires_in_seconds", "must be >= 0")
	}
	if input.ExpiresInSeconds > MaxRotationGraceSeconds {
		return RotateKeyResult{}, errs.Invalid("expires_in_seconds",
			fmt.Sprintf("must be <= %d (7 days)", MaxRotationGraceSeconds))
	}

	old, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return RotateKeyResult{}, err
	}
	if old.RevokedAt != nil {
		return RotateKeyResult{}, errs.InvalidState("cannot rotate a revoked key")
	}

	// Mint the replacement through the same CreateKey path so livemode
	// propagation, hashing, and prefix generation stay in one place. The new
	// key inherits name+type+expires_at from the old; a rotation is a
	// credential swap, not a reconfiguration.
	createInput := CreateKeyInput{
		Name:      old.Name,
		KeyType:   KeyType(old.KeyType),
		ExpiresAt: old.ExpiresAt,
	}
	// The new key must land on the same livemode as the old — Livemode(ctx)
	// depends on the request ctx, which the caller controls. We force the
	// ctx to match the old key's mode so a live key rotated from a test-mode
	// session (unlikely but possible through platform keys) still produces a
	// live replacement.
	newCtx := ctx
	if Livemode(ctx) != old.Livemode {
		newCtx = WithLivemode(ctx, old.Livemode)
	}
	created, err := s.CreateKey(newCtx, tenantID, createInput)
	if err != nil {
		return RotateKeyResult{}, fmt.Errorf("mint replacement key: %w", err)
	}

	var oldAfter domain.APIKey
	if input.ExpiresInSeconds == 0 {
		// Immediate revocation: old key stops authenticating on the next
		// ValidateKey call. Any request already in flight under the old key
		// completes; subsequent ones fail.
		oldAfter, err = s.store.Revoke(ctx, tenantID, id)
	} else {
		graceUntil := time.Now().UTC().Add(time.Duration(input.ExpiresInSeconds) * time.Second)
		oldAfter, err = s.store.ScheduleExpiry(ctx, tenantID, id, graceUntil)
	}
	if err != nil {
		// The new key is already minted. Leaving it live (and failing rotation)
		// means callers can retry the rotation using the new key — the old
		// stays valid until its retry succeeds. A garbage-collection job can
		// eventually reap the unused new key if the caller never retries.
		return RotateKeyResult{}, fmt.Errorf("retire old key (replacement %s was created and must be cleaned up if retry fails): %w", created.Key.ID, err)
	}

	return RotateKeyResult{
		OldKey: oldAfter,
		NewKey: created.Key,
		RawKey: created.RawKey,
	}, nil
}

func (s *Service) ListKeys(ctx context.Context, filter ListFilter) ([]domain.APIKey, error) {
	return s.store.List(ctx, filter)
}
