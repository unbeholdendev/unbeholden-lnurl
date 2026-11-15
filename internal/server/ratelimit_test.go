package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestRateLimiter_AllowsNormalTraffic(t *testing.T) {
	rl := newRateLimiter(10.0/60.0, 10)
	handler := rl.middleware(proxies(nil).clientIP, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("GET", "/health", nil)
		req.RemoteAddr = "1.2.3.4:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, rec.Code)
		}
	}
}

func TestRateLimiter_BlocksExcessiveTraffic(t *testing.T) {
	rl := newRateLimiter(1, 1)
	handler := rl.middleware(proxies(nil).clientIP, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "1.2.3.4:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: expected 429, got %d", rec.Code)
	}
}

func TestRateLimiter_PerIPIsolation(t *testing.T) {
	rl := newRateLimiter(1, 1)
	handler := rl.middleware(proxies(nil).clientIP, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "1.2.3.4:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	req2 := httptest.NewRequest("GET", "/test", nil)
	req2.RemoteAddr = "5.6.7.8:5678"
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("different IP should not be rate limited, got %d", rec2.Code)
	}
}

func TestRateLimiter_XForwardedFor(t *testing.T) {
	cases := map[string]struct {
		trusted []string
		xff     [2]string
		want    int
	}{
		"same client behind trusted proxy":  {[]string{"10.0.0.0/8"}, [2]string{"203.0.113.50", "203.0.113.50"}, http.StatusTooManyRequests},
		"other client behind trusted proxy": {[]string{"10.0.0.0/8"}, [2]string{"203.0.113.50", "203.0.113.51"}, http.StatusOK},
		"spoofed from untrusted peer":       {nil, [2]string{"203.0.113.50", "203.0.113.51"}, http.StatusTooManyRequests},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rl := newRateLimiter(1, 1)
			handler := rl.middleware(trustedProxies(t, tc.trusted...).clientIP, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
			var code int
			for _, xff := range tc.xff {
				req := httptest.NewRequest("GET", "/test", nil)
				req.RemoteAddr = "10.0.0.1:1234"
				req.Header.Set("X-Forwarded-For", xff)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				code = rec.Code
			}
			if code != tc.want {
				t.Fatalf("second request: got %d, want %d", code, tc.want)
			}
		})
	}
}

func TestRateLimiter_PrunesRefilledVisitors(t *testing.T) {
	rl := newRateLimiter(1.0/3600, 1)
	rl.limiters["1.2.3.4"] = rate.NewLimiter(rl.rate, rl.burst)
	if !rl.allow("5.6.7.8") {
		t.Fatal("first request refused")
	}
	rl.lastPrune = time.Now().Add(-pruneInterval - time.Second)

	rl.allow("9.9.9.9")
	if _, ok := rl.limiters["1.2.3.4"]; ok {
		t.Fatal("refilled visitor not pruned")
	}
	if rl.allow("5.6.7.8") {
		t.Fatal("pruning reset a visitor whose bucket had not refilled")
	}
}
