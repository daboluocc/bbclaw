package claudecode

import (
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// newTestPool creates a WarmPool with the background goroutine disabled
// (size=0 means no goroutine is started). Tests that need a live pool
// use newTestPoolLive.
func newTestPool(size int, ttl time.Duration) *WarmPool {
	return &WarmPool{
		bin:         "claude",
		size:        size,
		idleTTL:     ttl,
		log:         obs.NewLogger(),
		replenishCh: make(chan struct{}, 1),
		done:        make(chan struct{}),
	}
}

// TestAcquireDisabled verifies that a pool with size=0 always returns a miss.
func TestAcquireDisabled(t *testing.T) {
	p := newTestPool(0, 10*time.Minute)
	id, ok := p.Acquire("/some/cwd")
	if ok || id != "" {
		t.Errorf("disabled pool: want miss, got id=%q ok=%v", id, ok)
	}
}

// TestAcquireHit verifies that an injected entry is returned and removed.
func TestAcquireHit(t *testing.T) {
	p := newTestPool(1, 10*time.Minute)
	p.injectEntry(warmEntry{
		cliSessionID: "abc-123",
		cwd:          "",
		createdAt:    time.Now(),
	})

	id, ok := p.Acquire("/any/cwd")
	if !ok || id != "abc-123" {
		t.Errorf("want hit id=abc-123, got id=%q ok=%v", id, ok)
	}
	// Pool should now be empty.
	if p.Len() != 0 {
		t.Errorf("pool should be empty after acquire, got len=%d", p.Len())
	}
}

// TestAcquireMissOnCwdMismatch verifies that an entry with a specific cwd is
// not returned when the request cwd differs.
func TestAcquireMissOnCwdMismatch(t *testing.T) {
	p := newTestPool(2, 10*time.Minute)
	p.injectEntry(warmEntry{
		cliSessionID: "xyz-999",
		cwd:          "/project/a",
		createdAt:    time.Now(),
	})

	id, ok := p.Acquire("/project/b")
	if ok || id != "" {
		t.Errorf("cwd mismatch: want miss, got id=%q ok=%v", id, ok)
	}
	// Entry should still be in the pool.
	if p.Len() != 1 {
		t.Errorf("entry should remain in pool after cwd mismatch, got len=%d", p.Len())
	}
}

// TestAcquireCwdMatchExact verifies that an entry with a specific cwd IS
// returned when the request cwd matches exactly.
func TestAcquireCwdMatchExact(t *testing.T) {
	p := newTestPool(2, 10*time.Minute)
	p.injectEntry(warmEntry{
		cliSessionID: "exact-match",
		cwd:          "/project/a",
		createdAt:    time.Now(),
	})

	id, ok := p.Acquire("/project/a")
	if !ok || id != "exact-match" {
		t.Errorf("exact cwd match: want hit, got id=%q ok=%v", id, ok)
	}
}

// TestAcquireTTLExpired verifies that an entry older than idleTTL is skipped.
func TestAcquireTTLExpired(t *testing.T) {
	p := newTestPool(1, 5*time.Minute)
	p.injectEntry(warmEntry{
		cliSessionID: "stale-session",
		cwd:          "",
		createdAt:    time.Now().Add(-10 * time.Minute), // older than TTL
	})

	id, ok := p.Acquire("/any/cwd")
	if ok || id != "" {
		t.Errorf("expired entry: want miss, got id=%q ok=%v", id, ok)
	}
	// The stale entry is still in the slice (eviction happens in the background
	// loop, not in Acquire). Len should still be 1.
	if p.Len() != 1 {
		t.Errorf("stale entry should remain until eviction loop runs, got len=%d", p.Len())
	}
}

// TestAcquireTTLZeroNeverExpires verifies that TTL=0 disables expiry.
func TestAcquireTTLZeroNeverExpires(t *testing.T) {
	p := newTestPool(1, 0) // TTL=0 → no expiry
	p.injectEntry(warmEntry{
		cliSessionID: "old-but-valid",
		cwd:          "",
		createdAt:    time.Now().Add(-24 * time.Hour),
	})

	id, ok := p.Acquire("/any/cwd")
	if !ok || id != "old-but-valid" {
		t.Errorf("TTL=0: want hit, got id=%q ok=%v", id, ok)
	}
}

// TestAcquireConcurrent verifies that concurrent Acquire calls each get a
// distinct entry and the pool is not over-drained.
func TestAcquireConcurrent(t *testing.T) {
	p := newTestPool(3, 10*time.Minute)
	for i := 0; i < 3; i++ {
		p.injectEntry(warmEntry{
			cliSessionID: "sess-" + string(rune('A'+i)),
			cwd:          "",
			createdAt:    time.Now(),
		})
	}

	results := make(chan string, 5)
	for i := 0; i < 5; i++ {
		go func() {
			id, ok := p.Acquire("/cwd")
			if ok {
				results <- id
			} else {
				results <- ""
			}
		}()
	}

	hits := 0
	misses := 0
	for i := 0; i < 5; i++ {
		id := <-results
		if id != "" {
			hits++
		} else {
			misses++
		}
	}
	if hits != 3 {
		t.Errorf("want 3 hits (pool size), got hits=%d misses=%d", hits, misses)
	}
	if misses != 2 {
		t.Errorf("want 2 misses (pool exhausted), got misses=%d", misses)
	}
	if p.Len() != 0 {
		t.Errorf("pool should be empty after 3 hits, got len=%d", p.Len())
	}
}

// TestDrainClearsPool verifies that Drain empties the pool.
func TestDrainClearsPool(t *testing.T) {
	p := newTestPool(2, 10*time.Minute)
	p.injectEntry(warmEntry{cliSessionID: "a", createdAt: time.Now()})
	p.injectEntry(warmEntry{cliSessionID: "b", createdAt: time.Now()})

	p.Drain()

	if p.Len() != 0 {
		t.Errorf("after Drain, want len=0, got %d", p.Len())
	}
	// Acquire after Drain should always miss (pool is empty).
	id, ok := p.Acquire("/cwd")
	if ok || id != "" {
		t.Errorf("after Drain, want miss, got id=%q ok=%v", id, ok)
	}
}

// TestDrainIdempotent verifies that calling Drain multiple times is safe.
func TestDrainIdempotent(t *testing.T) {
	p := newTestPool(1, 10*time.Minute)
	p.Drain()
	p.Drain() // should not panic
}

// TestEvictExpired verifies that evictExpired removes stale entries.
func TestEvictExpired(t *testing.T) {
	p := newTestPool(3, 5*time.Minute)
	p.injectEntry(warmEntry{cliSessionID: "fresh", cwd: "", createdAt: time.Now()})
	p.injectEntry(warmEntry{cliSessionID: "stale", cwd: "", createdAt: time.Now().Add(-10 * time.Minute)})
	p.injectEntry(warmEntry{cliSessionID: "also-fresh", cwd: "", createdAt: time.Now().Add(-1 * time.Minute)})

	p.evictExpired()

	if p.Len() != 2 {
		t.Errorf("after eviction, want 2 entries, got %d", p.Len())
	}
	// The stale entry should be gone; fresh ones should remain.
	id, ok := p.Acquire("/cwd")
	if !ok {
		t.Fatal("expected a hit after eviction")
	}
	if id == "stale" {
		t.Errorf("stale entry should have been evicted, but got id=%q", id)
	}
}

// TestLenAndSize verify the accessor methods.
func TestLenAndSize(t *testing.T) {
	p := newTestPool(5, 10*time.Minute)
	if p.Size() != 5 {
		t.Errorf("Size: want 5, got %d", p.Size())
	}
	if p.Len() != 0 {
		t.Errorf("Len: want 0, got %d", p.Len())
	}
	p.injectEntry(warmEntry{cliSessionID: "x", createdAt: time.Now()})
	if p.Len() != 1 {
		t.Errorf("Len after inject: want 1, got %d", p.Len())
	}
}

// TestStripCCPrefix verifies the helper used by pool and driver.
func TestStripCCPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"cc-abc-123", "abc-123"},
		{"abc-123", "abc-123"},
		{"", ""},
		{"cc-", ""},
	}
	for _, c := range cases {
		got := stripCCPrefix(c.in)
		if got != c.want {
			t.Errorf("stripCCPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
