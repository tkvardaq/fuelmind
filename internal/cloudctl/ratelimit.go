package cloudctl

import (
	"sync"
	"time"
)

// Webhook rate limit: how many inbound messages one sender may send in a
// window before being told to wait. An owner asking their station a few
// questions never notices; a caller working through the number space
// does.
const (
	webhookBurst  = 10
	webhookWindow = time.Minute
	// limiterMaxKeys bounds memory: a flood from many distinct numbers
	// must not grow the map without limit.
	limiterMaxKeys = 10000
)

// rateLimiter is a fixed-window counter keyed by caller. It is not a
// precise token bucket — it does not need to be. It needs to make an
// unthrottled endpoint throttled, cheaply, without a dependency.
type rateLimiter struct {
	mu     sync.Mutex
	burst  int
	window time.Duration
	seen   map[string]*windowCount
}

type windowCount struct {
	start time.Time
	n     int
}

func newRateLimiter(burst int, window time.Duration) *rateLimiter {
	return &rateLimiter{burst: burst, window: window, seen: map[string]*windowCount{}}
}

// allow records one request from key and reports whether it is within
// the limit. An empty key is limited as its own bucket, so unidentified
// callers share one allowance rather than getting a free pass each.
func (l *rateLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.seen) >= limiterMaxKeys {
		for k, c := range l.seen {
			if now.Sub(c.start) >= l.window {
				delete(l.seen, k)
			}
		}
		// Still full: every bucket is live, so refuse rather than grow.
		if len(l.seen) >= limiterMaxKeys {
			if _, known := l.seen[key]; !known {
				return false
			}
		}
	}

	c, ok := l.seen[key]
	if !ok || now.Sub(c.start) >= l.window {
		l.seen[key] = &windowCount{start: now, n: 1}
		return true
	}
	c.n++
	return c.n <= l.burst
}

// webhookLimiter returns the server's webhook limiter, creating it on
// first use so a zero-valued Server still rate-limits.
func (s *Server) webhookLimiter() *rateLimiter {
	s.limiterOnce.Do(func() {
		s.limiter = newRateLimiter(webhookBurst, webhookWindow)
	})
	return s.limiter
}
