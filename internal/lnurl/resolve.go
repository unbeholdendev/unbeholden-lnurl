package lnurl

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg"

	"unbeholden-lnurl/internal/bolt11"
)

var addressUser = regexp.MustCompile(`^[a-z0-9._+-]+$`)

// Resolver fetches invoices from Lightning Addresses (LUD-16).
type Resolver struct {
	client  *http.Client
	proxied bool
	network *chaincfg.Params
}

// NewResolver returns a Resolver that accepts invoices for network and
// connects through proxy, if set. Onion addresses need a proxy.
func NewResolver(proxy *url.URL, network *chaincfg.Params) *Resolver {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != nil {
		transport.Proxy = http.ProxyURL(proxy)
	}
	return &Resolver{
		client: &http.Client{
			Transport:     transport,
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		proxied: proxy != nil,
		network: network,
	}
}

// Invoice returns an invoice for amountMsat from address, checked to be for
// that amount and to commit to the address's metadata.
func (r *Resolver) Invoice(ctx context.Context, address string, amountMsat int64, comment string) (string, error) {
	user, domain, ok := strings.Cut(strings.ToLower(address), "@")
	if !ok || !addressUser.MatchString(user) || domain == "" || strings.ContainsAny(domain, "/?#@\\") {
		return "", errors.New("invalid Lightning Address")
	}
	base, err := r.endpoint(hostURL(domain) + "/.well-known/lnurlp/" + user)
	if err != nil {
		return "", err
	}
	var pay struct {
		Tag            string `json:"tag"`
		Callback       string `json:"callback"`
		Metadata       string `json:"metadata"`
		MinSendable    int64  `json:"minSendable"`
		MaxSendable    int64  `json:"maxSendable"`
		CommentAllowed int    `json:"commentAllowed"`
	}
	if err := r.get(ctx, base, &pay); err != nil {
		return "", err
	}
	if pay.Tag != "payRequest" {
		return "", errors.New("address did not return a payRequest")
	}
	if amountMsat < pay.MinSendable || amountMsat > pay.MaxSendable {
		return "", fmt.Errorf("address accepts %d to %d msat", pay.MinSendable, pay.MaxSendable)
	}
	if len([]rune(comment)) > pay.CommentAllowed {
		return "", fmt.Errorf("address accepts comments of at most %d characters", pay.CommentAllowed)
	}
	callback, err := r.endpoint(pay.Callback)
	if err != nil {
		return "", fmt.Errorf("callback: %w", err)
	}
	query := callback.Query()
	query.Set("amount", strconv.FormatInt(amountMsat, 10))
	if comment != "" {
		query.Set("comment", comment)
	}
	callback.RawQuery = query.Encode()
	var invoice struct {
		PR string `json:"pr"`
	}
	if err := r.get(ctx, callback, &invoice); err != nil {
		return "", err
	}
	decoded, err := bolt11.Decode(invoice.PR, r.network)
	if err != nil {
		return "", fmt.Errorf("address returned an invalid invoice: %w", err)
	}
	if decoded.AmountMsat == nil || *decoded.AmountMsat != amountMsat {
		return "", errors.New("address returned an invoice for another amount")
	}
	if decoded.DescriptionHash == nil || *decoded.DescriptionHash != sha256.Sum256([]byte(pay.Metadata)) {
		return "", errors.New("address returned an invoice that does not commit to its metadata")
	}
	return invoice.PR, nil
}

// endpoint parses rawURL, which must use HTTPS, or HTTP to an onion host
// through the proxy.
func (r *Resolver) endpoint(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return nil, errors.New("invalid URL")
	}
	onion := strings.HasSuffix(strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")), ".onion")
	switch {
	case onion && !r.proxied:
		return nil, errors.New("onion addresses need outbound_proxy")
	case u.Scheme != "https" && !(onion && u.Scheme == "http"):
		return nil, errors.New("URL must use HTTPS")
	}
	return u, nil
}

func (r *Resolver) get(ctx context.Context, u *url.URL, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", u.Host, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("fetch %s: %w", u.Host, err)
	}
	var failure status
	if json.Unmarshal(body, &failure) == nil && failure.Status == "ERROR" {
		return fmt.Errorf("%s: %s", u.Host, failure.Reason)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", u.Host, resp.StatusCode)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s: invalid response: %w", u.Host, err)
	}
	return nil
}
