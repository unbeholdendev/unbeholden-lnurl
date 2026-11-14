package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"unbeholden-lnurl/internal/config"
	"unbeholden-lnurl/internal/lnd"
	"unbeholden-lnurl/internal/lnurl"
	"unbeholden-lnurl/internal/notifier"
	"unbeholden-lnurl/internal/server"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	healthcheck := flag.Bool("healthcheck", false, "run health check and exit")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	if *healthcheck {
		resp, err := http.Get("http://" + cfg.Listen + "/health")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}
	log.Printf("config loaded from %s", *configPath)

	lndClient, err := lnd.Connect(cfg.LND.Host, cfg.LND.TLSCert, cfg.LND.Macaroon)
	if err != nil {
		log.Fatalf("failed to connect to lnd: %v", err)
	}
	defer lndClient.Close()
	log.Printf("connected to lnd at %s", cfg.LND.Host)

	ntfy := notifier.NewNtfy(cfg.Notifications.Ntfy)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var wg sync.WaitGroup
	if cfg.Notifies() {
		wg.Go(func() {
			notifier.WatchSettled(ctx, lndClient, filepath.Join(cfg.StateDir, "settle_index"), ntfy)
		})
	}

	if cfg.ServesHTTP() {
		serve(ctx, cfg, lndClient, ntfy)
	}

	<-ctx.Done()
	log.Println("shutting down")
	wg.Wait()
}

func serve(ctx context.Context, cfg *config.Config, lndClient *lnd.Client, ntfy *notifier.Ntfy) {
	withdrawHandler := lnurl.NewWithdrawHandler(
		cfg.ExpandVouchers(),
		lndClient,
		ntfy,
		filepath.Join(cfg.StateDir, "state.json"),
	)
	addressHandler := lnurl.NewAddressHandler(cfg.Addresses, lndClient, ntfy)

	if withdrawHandler != nil {
		unclaimed := withdrawHandler.UnclaimedVouchers()
		log.Printf("withdraw vouchers: %d unclaimed", len(unclaimed))
		for _, v := range unclaimed {
			log.Printf("  %s (%d sats) %s", v.ID, v.Amount/1000, v.URL)
			log.Printf("    %s", v.LNURL)
		}
		go withdrawHandler.StartCleanup(ctx)
	}

	srv := server.New(cfg, withdrawHandler, addressHandler)

	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}
