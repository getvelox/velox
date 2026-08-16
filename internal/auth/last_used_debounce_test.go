package auth

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests for the last_used_at debounce (#818).
//
// The failure these pin is not a wrong value — it is a wrong NUMBER of writes.
// Before the debounce every validated request spawned a goroutine that ran
// `UPDATE api_keys SET last_used_at` against the one row for that key, and on
// the AWS rig that single row lock capped one API key at ~570 requests/s on any
// hardware. So the assertions here count writes.

// countingStore wraps memStore and counts TouchLastUsed calls, per key.
type countingStore struct {
	*memStore
	touches sync.Map // key id -> *atomic.Int64
	total   atomic.Int64
}

func (c *countingStore) TouchLastUsed(ctx context.Context, id string, usedAt time.Time) error {
	v, _ := c.touches.LoadOrStore(id, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
	c.total.Add(1)
	return c.memStore.TouchLastUsed(ctx, id, usedAt)
}

func (c *countingStore) count(id string) int64 {
	v, ok := c.touches.Load(id)
	if !ok {
		return 0
	}
	return v.(*atomic.Int64).Load()
}

// settle waits for the async touch goroutines to land: the count must be
// stable for a short window. Bounded so a bug cannot hang the test.
func settle(t *testing.T, read func() int64) int64 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	last := read()
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		cur := read()
		if cur != last {
			last, stableSince = cur, time.Now()
			continue
		}
		if time.Since(stableSince) > 150*time.Millisecond {
			return cur
		}
	}
	return last
}

func newDebounceFixture(t *testing.T) (*Service, *countingStore, string, *time.Time) {
	t.Helper()
	store := &countingStore{memStore: newMemStore()}
	svc := NewService(store)
	// Injected clock so the interval can be crossed without sleeping a minute.
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	res, err := svc.CreateKey(context.Background(), "t1", CreateKeyInput{Name: "k", KeyType: KeyTypeSecret})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	// CreateKey may itself touch nothing; reset counters to be safe.
	store.touches = sync.Map{}
	store.total.Store(0)
	return svc, store, res.RawKey, &now
}

// TestLastUsed_DebouncedWithinInterval: N validations inside one interval must
// produce exactly ONE write. This is the assertion that fails on the old code
// (N writes) — the hot-row bug in its unit-test form.
func TestLastUsed_DebouncedWithinInterval(t *testing.T) {
	svc, store, raw, _ := newDebounceFixture(t)
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		if _, err := svc.ValidateKey(ctx, raw); err != nil {
			t.Fatalf("validate %d: %v", i, err)
		}
	}
	if got := settle(t, store.total.Load); got != 1 {
		t.Fatalf("100 validations within one interval issued %d last_used_at writes, want exactly 1 (this is #818: N writes = N goroutines queueing on one row)", got)
	}
}

// TestLastUsed_TouchesAgainAfterInterval: once the interval has elapsed the
// next validation writes again — the debounce must not turn into "never".
func TestLastUsed_TouchesAgainAfterInterval(t *testing.T) {
	svc, store, raw, now := newDebounceFixture(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		_, _ = svc.ValidateKey(ctx, raw)
	}
	if got := settle(t, store.total.Load); got != 1 {
		t.Fatalf("before interval: %d writes, want 1", got)
	}
	*now = now.Add(svc.touchInterval + time.Second)
	for i := 0; i < 10; i++ {
		_, _ = svc.ValidateKey(ctx, raw)
	}
	if got := settle(t, store.total.Load); got != 2 {
		t.Fatalf("after the interval elapsed: %d writes total, want exactly 2", got)
	}
	// And the value written is the later instant, not the first.
	k, _ := store.memStore.Get(ctx, "t1", svc.mustKeyID(t, ctx, raw))
	if k.LastUsedAt == nil || !k.LastUsedAt.Equal(*now) {
		t.Fatalf("last_used_at = %v, want %v (the most recent touch)", k.LastUsedAt, *now)
	}
}

// TestLastUsed_ConcurrentBurstWritesOnce: 200 goroutines validating the same
// key at the same instant must produce ONE write. The CAS on the per-key
// timestamp is what makes this exactly-once rather than "usually once".
func TestLastUsed_ConcurrentBurstWritesOnce(t *testing.T) {
	svc, store, raw, _ := newDebounceFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = svc.ValidateKey(ctx, raw)
		}()
	}
	close(start)
	wg.Wait()
	if got := settle(t, store.total.Load); got != 1 {
		t.Fatalf("200 concurrent validations issued %d writes, want exactly 1", got)
	}
}

// TestLastUsed_TouchPrimitiveIsExactlyOnceUnderRace drives touchLastUsed itself
// from many goroutines released on one barrier, repeatedly, and requires that
// each round issues exactly one write. Going through ValidateKey serialises
// the goroutines enough (store lock, hashing) that they never actually meet at
// the compare-and-swap, and a mutant that replaced the CAS with a plain store
// survived that test. This one is written to make them meet — and it does so
// reliably only under the race detector, which is how CI runs the unit suite
// (`go test -race`); without -race the goroutines rarely overlap in the
// nanosecond window between load and store. Mutation-verified: the plain-store
// mutant produced 2 writes in round 1 under -race.
func TestLastUsed_TouchPrimitiveIsExactlyOnceUnderRace(t *testing.T) {
	store := &countingStore{memStore: newMemStore()}
	svc := NewService(store)
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	round := 0
	svc.now = func() time.Time { return base.Add(time.Duration(round) * (svc.touchInterval + time.Second)) }
	const goroutines = 512
	for round = 0; round < 40; round++ {
		before := store.total.Load()
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; svc.touchLastUsed("k") }()
		}
		close(start)
		wg.Wait()
		got := settle(t, store.total.Load) - before
		if got != 1 {
			t.Fatalf("round %d: %d goroutines racing the touch primitive issued %d writes, want exactly 1", round, goroutines, got)
		}
	}
}

// TestLastUsed_PerKeyNotGlobal: the debounce is per key. Two keys validated in
// the same interval each get their own write; one key's touch must not
// suppress another's.
func TestLastUsed_PerKeyNotGlobal(t *testing.T) {
	svc, store, rawA, _ := newDebounceFixture(t)
	ctx := context.Background()
	resB, err := svc.CreateKey(ctx, "t1", CreateKeyInput{Name: "k2", KeyType: KeyTypeSecret})
	if err != nil {
		t.Fatal(err)
	}
	store.touches = sync.Map{}
	store.total.Store(0)
	for i := 0; i < 20; i++ {
		_, _ = svc.ValidateKey(ctx, rawA)
		_, _ = svc.ValidateKey(ctx, resB.RawKey)
	}
	if got := settle(t, store.total.Load); got != 2 {
		t.Fatalf("two keys in one interval: %d writes, want exactly 2 (one per key)", got)
	}
}

// mustKeyID resolves a raw key to its id via the store (test helper).
func (s *Service) mustKeyID(t *testing.T, ctx context.Context, raw string) string {
	t.Helper()
	k, err := s.ValidateKey(ctx, raw)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return k.ID
}
