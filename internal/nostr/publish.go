package nostr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	socksproxy "golang.org/x/net/proxy"
)

const publishTimeout = 10 * time.Second

// Publisher sends events to relays.
type Publisher struct {
	dialer *websocket.Dialer
}

// NewPublisher returns a Publisher that connects only to public addresses,
// since relays are named by whoever sends a zap request. With a proxy,
// address filtering and name resolution are left to the proxy.
func NewPublisher(proxy ...*url.URL) *Publisher {
	netDialer := &net.Dialer{Timeout: publishTimeout, Control: publicOnly}
	dialer := &websocket.Dialer{
		NetDialContext:   netDialer.DialContext,
		HandshakeTimeout: publishTimeout,
	}
	if len(proxy) > 0 && proxy[0] != nil {
		netDialer.Control = nil
		dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			d, err := socksproxy.FromURL(proxy[0], netDialer)
			if err != nil {
				return nil, err
			}
			contextual, ok := d.(socksproxy.ContextDialer)
			if !ok {
				return nil, errors.New("proxy does not support context-aware dialing")
			}
			return contextual.DialContext(ctx, network, address)
		}
	}
	return &Publisher{dialer: dialer}
}

var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

func publicOnly(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	ip := ap.Addr().Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || sharedAddressSpace.Contains(ip) {
		return fmt.Errorf("%s is not a public address", ip)
	}
	return nil
}

// Publish sends e to each wss:// relay in relays and returns how many
// accepted it.
func (p *Publisher) Publish(ctx context.Context, relays []string, e Event) int {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
	)
	for _, relay := range relays {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.publish(ctx, relay, e); err != nil {
				slog.Warn("publish to relay failed", "relay", relay, "event", e.ID, "err", err)
				return
			}
			mu.Lock()
			accepted++
			mu.Unlock()
		}()
	}
	wg.Wait()
	return accepted
}

func (p *Publisher) publish(ctx context.Context, relay string, e Event) error {
	if u, err := url.Parse(relay); err != nil || u.Scheme != "wss" {
		return errors.New("not a wss:// URL")
	}
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	conn, _, err := p.dialer.DialContext(ctx, relay, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetReadLimit(64 << 10)
	deadline, _ := ctx.Deadline()
	conn.SetReadDeadline(deadline)
	conn.SetWriteDeadline(deadline)

	if err := conn.WriteJSON([]any{"EVENT", e}); err != nil {
		return err
	}
	for {
		var msg []json.RawMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return err
		}
		var label, id string
		if len(msg) < 3 || json.Unmarshal(msg[0], &label) != nil || label != "OK" ||
			json.Unmarshal(msg[1], &id) != nil || id != e.ID {
			continue
		}
		var ok bool
		var reason string
		json.Unmarshal(msg[2], &ok)
		if len(msg) > 3 {
			json.Unmarshal(msg[3], &reason)
		}
		if !ok {
			return fmt.Errorf("rejected: %s", reason)
		}
		return nil
	}
}
