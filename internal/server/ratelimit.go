package server

import (
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	pruneInterval = 3 * time.Minute
	idleTTL       = 5 * time.Minute
)

type rateLimiter struct {
	mu        sync.Mutex
	limiters  map[string]*visitorLimiter
	rate      rate.Limit
	burst     int
	lastPrune time.Time
}

type visitorLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newRateLimiter(rps float64, burst int) *rateLimiter {
	return &rateLimiter{
		limiters:  make(map[string]*visitorLimiter),
		rate:      rate.Limit(rps),
		burst:     burst,
		lastPrune: time.Now(),
	}
}

func (rl *rateLimiter) getLimiter(ip string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	if now.Sub(rl.lastPrune) > pruneInterval {
		for ip, v := range rl.limiters {
			if now.Sub(v.lastSeen) > idleTTL {
				delete(rl.limiters, ip)
			}
		}
		rl.lastPrune = now
	}

	v, exists := rl.limiters[ip]
	if !exists {
		v = &visitorLimiter{limiter: rate.NewLimiter(rl.rate, rl.burst)}
		rl.limiters[ip] = v
	}
	v.lastSeen = now
	return v.limiter
}

func (rl *rateLimiter) middleware(clientIP func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.getLimiter(clientIP(r)).Allow() {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
