package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"golang.org/x/sync/errgroup"

	"unbeholden-lnurl/internal/cln"
	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/lnd"
	"unbeholden-lnurl/internal/lnurl"
	"unbeholden-lnurl/internal/notifier"
	"unbeholden-lnurl/internal/server"
)

const shutdownTimeout = lnurl.PayTimeout + 10*time.Second

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	healthcheck := flag.Bool("healthcheck", false, "run health check and exit")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}
	if *healthcheck {
		if err := checkHealth(cfg.Listen); err != nil {
			slog.Error("health check", "err", err)
			os.Exit(1)
		}
		return
	}
	slog.Info("config loaded", "path", *configPath)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err = run(ctx, cfg)
	stop()
	if err != nil {
		slog.Error("stopped", "err", err)
		os.Exit(1)
	}
}

func checkHealth(listen string) error {
	resp, err := http.Get("http://" + listen + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check: status %d", resp.StatusCode)
	}
	return nil
}

// node is a Lightning node backend.
type node interface {
	lnurl.Invoicer
	lnurl.Payer
	notifier.Invoices
	Network(ctx context.Context) (*chaincfg.Params, error)
	Close() error
}

func connect(cfg *config.Config) (node, error) {
	if c := cfg.CLN; c != nil {
		client, err := cln.Connect(c.URL, c.RuneFile, c.CACert)
		if err != nil {
			return nil, fmt.Errorf("cln: %w", err)
		}
		slog.Info("using cln", "url", c.URL)
		return client, nil
	}
	l := cfg.LND
	client, err := lnd.Connect(l.Host, l.TLSCert, l.Macaroon)
	if err != nil {
		return nil, fmt.Errorf("lnd: %w", err)
	}
	slog.Info("using lnd", "host", l.Host)
	return client, nil
}

func run(ctx context.Context, cfg *config.Config) error {
	node, err := connect(cfg)
	if err != nil {
		return err
	}
	defer node.Close()

	var ntfy *notifier.Ntfy
	// Kept apart from ntfy: a nil *Ntfy in an interface is not a nil Notifier.
	var notify lnurl.Notifier
	if n := cfg.Notifications.Ntfy; n != nil {
		ntfy = notifier.NewNtfy(*n)
		notify = ntfy
	}

	g, ctx := errgroup.WithContext(ctx)
	if cfg.ServesHTTP() {
		srv, err := newServer(ctx, cfg, node, notify)
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			return err
		}
		slog.Info("listening", "addr", ln.Addr().String())
		g.Go(func() error { return serve(ctx, srv, ln) })
	}
	if ntfy != nil {
		g.Go(func() error {
			// Named before Core Lightning support; a new name would lose the
			// position of existing deployments.
			notifier.WatchSettled(ctx, node, filepath.Join(cfg.StateDir, "settle_index"), ntfy)
			return nil
		})
	}
	return g.Wait()
}

func newServer(ctx context.Context, cfg *config.Config, node node, notify lnurl.Notifier) (*http.Server, error) {
	var withdrawHandler *lnurl.WithdrawHandler
	if len(cfg.Withdraw) > 0 {
		network, err := node.Network(ctx)
		if err != nil {
			return nil, fmt.Errorf("get node network: %w", err)
		}
		withdrawHandler, err = lnurl.NewWithdrawHandler(
			cfg.ExpandVouchers(),
			cfg.WithdrawK1Key(),
			network,
			node,
			notify,
			filepath.Join(cfg.StateDir, "state.json"),
		)
		if err != nil {
			return nil, err
		}
		unclaimed := withdrawHandler.UnclaimedVouchers()
		slog.Info("withdraw vouchers", "unclaimed", len(unclaimed))
		for _, v := range unclaimed {
			slog.Info("unclaimed voucher", "id", v.ID, "sats", v.Amount/1000, "url", v.URL, "lnurl", v.LNURL)
		}
	}
	var addressHandler *lnurl.AddressHandler
	if len(cfg.Addresses) > 0 {
		verifyKey, err := cfg.VerifyKey()
		if err != nil {
			return nil, err
		}
		addressHandler = lnurl.NewAddressHandler(cfg.Addresses, node, notify, verifyKey)
	}
	return server.New(cfg.TrustedProxies, withdrawHandler, addressHandler), nil
}

// serve serves srv on ln until ctx is done, then shuts it down, waiting for
// requests in flight, such as a withdraw payment.
func serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down", "wait_for_requests", shutdownTimeout)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
