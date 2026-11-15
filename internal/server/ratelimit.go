package server

import (
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const pruneInterval = 3 * time.Minute

type rateLimiter struct {
	mu        sync.Mutex
	limiters  map[string]*rate.Limiter
	rate      rate.Limit
	burst     int
	lastPrune time.Time
}

func newRateLimiter(rps float64, burst int) *rateLimiter {
	return &rateLimiter{
		limiters:  make(map[string]*rate.Limiter),
		rate:      rate.Limit(rps),
		burst:     burst,
		lastPrune: time.Now(),
	}
}

// allow takes a token for ip. It forgets limiters whose bucket has refilled,
// as a new one would start the same, so it takes the token under the same
// lock as pruning.
func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	if now.Sub(rl.lastPrune) > pruneInterval {
		for ip, l := range rl.limiters {
			if l.TokensAt(now) >= float64(rl.burst) {
				delete(rl.limiters, ip)
			}
		}
		rl.lastPrune = now
	}

	l, exists := rl.limiters[ip]
	if !exists {
		l = rate.NewLimiter(rl.rate, rl.burst)
		rl.limiters[ip] = l
	}
	return l.AllowN(now, 1)
}

func (rl *rateLimiter) middleware(clientIP func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(clientIP(r)) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
