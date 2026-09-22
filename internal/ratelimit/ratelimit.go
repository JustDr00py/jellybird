// Package ratelimit provides a token-bucket limiter with 429-aware backoff for
// outbound debrid API calls.
package ratelimit

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// Limiter is a token bucket refilling at rate permits per minute.
type Limiter struct {
	mu      sync.Mutex
	tokens  float64
	max     float64
	refill  float64 // tokens per second
	last    time.Time
	maxWait time.Duration
}

// New returns a Limiter allowing requestsPerMinute with a bounded wait.
func New(requestsPerMinute int) *Limiter {
	if requestsPerMinute <= 0 {
		requestsPerMinute = 60
	}
	return &Limiter{
		tokens:  float64(requestsPerMinute),
		max:     float64(requestsPerMinute),
		refill:  float64(requestsPerMinute) / 60.0,
		maxWait: time.Minute,
	}
}

// Wait blocks until a token is available, the wait budget is exceeded, or ctx
// expires.
func (l *Limiter) Wait(ctx context.Context) error {
	deadline := time.Now().Add(l.maxWait)
	for {
		l.mu.Lock()
		now := time.Now()
		if l.last.IsZero() {
			l.last = now
		}
		l.tokens = min(l.max, l.tokens+now.Sub(l.last).Seconds()*l.refill)
		l.last = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - l.tokens) / l.refill * float64(time.Second))
		l.mu.Unlock()

		if now.Add(wait).After(deadline) {
			return fmt.Errorf("rate limiter wait %s exceeds budget", wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait + 10*time.Millisecond):
		}
	}
}

// Backoff sleeps a jittered exponential delay after an HTTP 429, honoring ctx.
// retryAfter, when positive, takes precedence.
func Backoff(ctx context.Context, attempt int, retryAfter time.Duration) error {
	var d time.Duration
	if retryAfter > 0 {
		d = retryAfter
	} else {
		d = time.Duration(1<<min(attempt, 5)) * time.Second
	}
	// Jitter ±20% so concurrent workers don't thunder-herd.
	jitter := time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(jitter):
		return nil
	}
}
