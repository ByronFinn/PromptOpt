package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// TestRateLimiterPacesConcurrentWaits pins the concurrency contract:
// the n-th concurrent Wait reserves the n-th slot, so N callers need
// at least (N-1)/rps wall time to pass. Generous tolerance absorbs
// scheduler jitter; the point is that the callers cannot burst through.
func TestRateLimiterPacesConcurrentWaits(t *testing.T) {
	const rps, n = 100, 8 // interval 10ms → lower bound ≈ 70ms
	l := newRateLimiter(rps, time.Now)
	if l == nil {
		t.Fatal("newRateLimiter(100) = nil")
	}
	start := time.Now()
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if err := l.Wait(context.Background()); err != nil {
				t.Errorf("Wait: %v", err)
			}
		})
	}
	wg.Wait()
	elapsed := time.Since(start)
	if min := time.Duration(n-1) * time.Second / rps; elapsed < min-20*time.Millisecond {
		t.Errorf("%d waits elapsed in %v, want at least ~%v (pacing ignored)", n, elapsed, min)
	}
	// A single grant stays immediate: the first caller never waits.
	l2 := newRateLimiter(rps, time.Now)
	begin := time.Now()
	if err := l2.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if d := time.Since(begin); d > 20*time.Millisecond {
		t.Errorf("first Wait took %v, want immediate", d)
	}
	// rps <= 0 builds no limiter at all (off is the default).
	if newRateLimiter(0, time.Now) != nil || newRateLimiter(-1, time.Now) != nil {
		t.Error("rps <= 0 must build no limiter")
	}
}

// TestChatRateLimitCoexistsWithRetryAfter stacks the two mechanisms:
// pacing is on, the first attempt answers 429 (server-driven backoff
// via Retry-After), the retry slips through the same limiter and
// succeeds — the limiter gates every attempt while the retry policy
// still owns the inter-attempt waits.
func TestChatRateLimitCoexistsWithRetryAfter(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, completionBody("", "ok", 1, 1))
	}))
	defer srv.Close()

	c, waits := newTestClient(t, srv.URL, OpenAIConfig{MaxRPS: 5000, Backoff: 10 * time.Millisecond})
	resp, err := c.Chat(context.Background(), ChatRequest{Role: core.RoleExecutor})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2 (retry slipped through the limiter)", hits.Load())
	}
	// The 429's Retry-After wait still comes from the retry policy.
	if len(*waits) != 1 || (*waits)[0] != time.Second {
		t.Errorf("waits = %v, want [1s]", *waits)
	}
}
