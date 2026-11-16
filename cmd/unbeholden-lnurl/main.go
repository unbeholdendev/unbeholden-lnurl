package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"golang.org/x/sync/errgroup"

	"unbeholden-lnurl/internal/cln"
	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/lightning"
	"unbeholden-lnurl/internal/lnd"
	"unbeholden-lnurl/internal/lnurl"
	"unbeholden-lnurl/internal/nostr"
	"unbeholden-lnurl/internal/notifier"
	"unbeholden-lnurl/internal/server"
	"unbeholden-lnurl/internal/zap"
)

const shutdownTimeout = lnurl.PayTimeout + 10*time.Second

var version string

func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	diagnostics := flag.Bool("diagnose", false, "check files, state permissions and read-only node access, then exit")
	publicURL := flag.String("public-url", "", "public LNURL-pay metadata URL to check with -diagnose")
	healthcheck := flag.Bool("healthcheck", false, "run health check and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	listVouchers := flag.Bool("list-vouchers", false, "print unclaimed vouchers as JSON and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(buildVersion())
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if (*publicURL != "" && !*diagnostics) || (*diagnostics && (*healthcheck || *listVouchers)) {
		slog.Error("use -public-url with -diagnose; do not combine -diagnose with -healthcheck or -list-vouchers")
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}
	if *diagnostics {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := diagnose(ctx, cfg, *publicURL, os.Stdout); err != nil {
			os.Exit(1)
		}
		return
	}
	if *listVouchers {
		handler, err := lnurl.NewWithdrawHandler(cfg.ExpandVouchers(), cfg.WithdrawK1Key(), nil, nil, nil, filepath.Join(cfg.StateDir, "state.json"))
		if err == nil {
			err = json.NewEncoder(os.Stdout).Encode(handler.UnclaimedVouchers())
		}
		if err != nil {
			slog.Error("list vouchers", "err", err)
			os.Exit(1)
		}
		return
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
		ntfy = notifier.NewNtfy(*n, cfg.ProxyURL)
		notify = ntfy
	}

	var zapper *zap.Zapper
	if n := cfg.Nostr; n != nil {
		key, err := nostr.ParseKey(n.PrivateKey)
		if err != nil {
			return fmt.Errorf("nostr.private_key_file: %w", err)
		}
		zapper = zap.New(key, nostr.NewPublisher(cfg.ProxyURL), filepath.Join(cfg.StateDir, "zaps"))
		slog.Info("zaps enabled", "nostr_pubkey", zapper.PubKey())
	}

	var payment, receipt func(lightning.Settled) error
	if ntfy != nil {
		payment = ntfy.NotifyPayment
	}
	if zapper != nil {
		receipt = func(invoice lightning.Settled) error { return zapper.Settled(ctx, invoice) }
	}
	watchers, err := prepareSettlements(ctx, node, cfg.StateDir, payment, receipt)
	if err != nil {
		return fmt.Errorf("initialise settlement tracking: %w", err)
	}
	g, ctx := errgroup.WithContext(ctx)
	if cfg.ServesHTTP() {
		srv, err := newServer(ctx, cfg, node, notify, zapper)
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
	if zapper != nil {
		g.Go(func() error {
			ticker := time.NewTicker(time.Hour)
			defer ticker.Stop()
			for {
				zapper.Prune(ctx, node)
				select {
				case <-ctx.Done():
					return nil
				case <-ticker.C:
				}
			}
		})
	}
	for _, watcher := range watchers {
		g.Go(func() error {
			notifier.WatchSettled(ctx, node, watcher.path, watcher.handle)
			return nil
		})
	}
	return g.Wait()
}

func newServer(ctx context.Context, cfg *config.Config, node node, notify lnurl.Notifier, zapper *zap.Zapper) (*http.Server, error) {
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
	}
	var addressHandler *lnurl.AddressHandler
	if len(cfg.Addresses) > 0 {
		verifyKey, err := cfg.VerifyKey()
		if err != nil {
			return nil, err
		}
		// Kept apart from zapper: a nil *Zapper in an interface is not a nil Zapper.
		var zaps lnurl.Zapper
		if zapper != nil {
			zaps = zapper
		}
		addressHandler = lnurl.NewAddressHandler(cfg.Addresses, node, notify, verifyKey, zaps)
	}
	return server.New(cfg.TrustedProxies, cfg.RateLimit, withdrawHandler, addressHandler), nil
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

type settlementWatcher struct {
	path   string
	handle func(lightning.Settled) error
}

func prepareSettlements(ctx context.Context, invoices notifier.Invoices, dir string, payment, receipt func(lightning.Settled) error) ([]settlementWatcher, error) {
	if payment == nil && receipt == nil {
		return nil, nil
	}
	path := filepath.Join(dir, "settle_index")
	if err := notifier.InitSettled(ctx, invoices, path); err != nil {
		return nil, err
	}
	var watchers []settlementWatcher
	if receipt != nil {
		zapPath := filepath.Join(dir, "zap_settle_index")
		if err := notifier.ForkSettled(zapPath, path); err != nil {
			return nil, err
		}
		watchers = append(watchers, settlementWatcher{zapPath, receipt})
	}
	if payment != nil {
		watchers = append(watchers, settlementWatcher{path, payment})
	}
	return watchers, nil
}
