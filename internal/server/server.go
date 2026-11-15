package server

import (
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/lnurl"
)

const withdrawPath = "/lnurl/withdraw/"

// New returns an HTTP server for the given handlers, either of which may be
// nil. It honours forwarding headers only on requests from trustedProxies.
func New(trustedProxies []config.Prefix, withdraw *lnurl.WithdrawHandler, address *lnurl.AddressHandler) *http.Server {
	mux := http.NewServeMux()
	if withdraw != nil {
		mux.HandleFunc("GET "+withdrawPath+"{token}", withdraw.HandleFirst)
		mux.HandleFunc("GET "+withdrawPath+"{token}/claim", withdraw.HandleClaim)
	}
	if address != nil {
		mux.HandleFunc("GET /.well-known/lnurlp/{username}", address.HandleWellKnown)
		mux.HandleFunc("GET /.well-known/lnurlp/{username}/invoice", address.HandleInvoice)
		mux.HandleFunc("GET /.well-known/lnurlp/{username}/verify/{token}", address.HandleVerify)
	}
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	p := proxies(trustedProxies)
	var handler http.Handler = requestLogging(slog.Default(), p.clientIP, mux)
	handler = newRateLimiter(10.0/60.0, 10).middleware(p.clientIP, handler)
	handler = p.forwardedHost(handler)
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      lnurl.PayTimeout + 10*time.Second,
		IdleTimeout:       time.Minute,
	}
}

type proxies []config.Prefix

func (p proxies) trusts(addr netip.Addr) bool {
	return slices.ContainsFunc(p, func(t config.Prefix) bool { return t.Contains(addr) })
}

func peer(r *http.Request) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	return ap.Addr().Unmap(), err == nil
}

// clientIP is the peer's address or, if the peer is a trusted proxy, the
// right-most X-Forwarded-For address that is not.
func (p proxies) clientIP(r *http.Request) string {
	addr, ok := peer(r)
	if !ok {
		return r.RemoteAddr
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0 && p.trusts(addr); i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		addr = hop.Unmap()
	}
	return addr.String()
}

// forwardedHost sets the request's Host from X-Forwarded-Host when a trusted
// proxy sent it. Apps that proxy with their own HTTP client can't keep Host,
// so they send it this way.
func (p proxies) forwardedHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			if addr, ok := peer(r); ok && p.trusts(addr) {
				r = r.Clone(r.Context())
				r.Host = h
			}
		}
		next.ServeHTTP(w, r)
	})
}
