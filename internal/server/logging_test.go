package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func logRequest(t *testing.T, pattern, method, target string, status int) string {
	t.Helper()
	var buf bytes.Buffer
	mux := http.NewServeMux()
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	})
	handler := requestLogging(slog.New(slog.NewTextHandler(&buf, nil)), proxies(nil).clientIP, mux)

	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = "1.2.3.4:5678"
	handler.ServeHTTP(httptest.NewRecorder(), req)
	return buf.String()
}

func TestRequestLogging_LogsRequestDetails(t *testing.T) {
	line := logRequest(t, "GET /health", "GET", "/health", http.StatusOK)
	for _, want := range []string{"method=GET", "path=/health", "status=200", "duration=", "client=1.2.3.4"} {
		if !strings.Contains(line, want) {
			t.Errorf("log should contain %s, got: %s", want, line)
		}
	}
}

func TestRequestLogging_CapturesNon200Status(t *testing.T) {
	if line := logRequest(t, "GET /missing", "GET", "/missing", http.StatusNotFound); !strings.Contains(line, "status=404") {
		t.Errorf("log should contain 404 status, got: %s", line)
	}
}

func TestRequestLogging_HidesVoucherToken(t *testing.T) {
	const pattern = "GET /lnurl/withdraw/{token}/claim"
	for _, tc := range []struct{ method, target, want string }{
		{"GET", "/lnurl/withdraw/0123abcd/claim?k1=ff", "path=/lnurl/withdraw/{token}/claim"},
		{"POST", "/lnurl/withdraw/0123abcd/claim", "path=/lnurl/withdraw/{token}/claim"},
		{"GET", "/lnurl/withdraw/0123abcd", "path=/lnurl/withdraw/{token}"},
		{"GET", "/lnurl/withdraw/0123abcd/claim/extra", "path=/lnurl/withdraw/{token}/claim/extra"},
	} {
		line := logRequest(t, pattern, tc.method, tc.target, http.StatusOK)
		if strings.Contains(line, "0123abcd") || strings.Contains(line, "k1") || !strings.Contains(line, tc.want+" ") {
			t.Errorf("%s %s: log should hide the token, got: %s", tc.method, tc.target, line)
		}
	}
}

func TestStatusResponseWriter_DefaultStatus(t *testing.T) {
	rw := &statusResponseWriter{ResponseWriter: httptest.NewRecorder()}
	rw.Write([]byte("hello"))
	if rw.statusCode() != http.StatusOK {
		t.Errorf("expected default status 200, got %d", rw.statusCode())
	}
}
