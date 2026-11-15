package server

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// statusResponseWriter wraps http.ResponseWriter to capture the status code.
type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusResponseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// requestLogging returns middleware that logs each request's method, path,
// status, duration and client IP. It hides voucher tokens in the path.
func requestLogging(logger *slog.Logger, clientIP func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusResponseWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		logger.Info("request",
			"method", r.Method,
			"path", hideToken(r.URL.Path),
			"status", sw.statusCode(),
			"duration", time.Since(start).Round(time.Millisecond),
			"client", clientIP(r),
		)
	})
}

func hideToken(path string) string {
	rest, ok := strings.CutPrefix(path, withdrawPath)
	if !ok || rest == "" {
		return path
	}
	_, tail, found := strings.Cut(rest, "/")
	if found {
		tail = "/" + tail
	}
	return withdrawPath + "{token}" + tail
}
