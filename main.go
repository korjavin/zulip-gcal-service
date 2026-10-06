// Command zulip-gcal-service: Google Calendar reminders as Zulip DMs.
// See docs/design.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/korjavin/zulip-gcal-service/internal/auth"
	"github.com/korjavin/zulip-gcal-service/internal/config"
	"github.com/korjavin/zulip-gcal-service/internal/link"
	"github.com/korjavin/zulip-gcal-service/internal/store"
	"github.com/korjavin/zulip-gcal-service/internal/zulip"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck("http://127.0.0.1:8080/healthz"))
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err) // plain text: multi-line config problems stay readable
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		// Names every problem; values are never included.
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "zulip-gcal.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	// ponytail: Zulip unreachable at startup is fatal too; the container's
	// restart policy retries.
	zc := zulip.New(cfg.ZulipSite, cfg.ZulipBotEmail, cfg.ZulipBotAPIKey)
	botID, err := zc.CheckServer(ctx)
	if err != nil {
		return err
	}
	mux := routes(st)
	au := auth.New(cfg, st, auth.Google)
	au.Register(mux)
	var bot *zulip.Bot
	// ponytail: Poll stays nil until the poller (zgc-lvo.1) exists.
	lk := &link.Linker{St: st, Zulip: zc, Account: au.Account, BotHealthy: func() bool { return bot.Healthy() },
		BotID: botID, PublicURL: cfg.PublicURL, ZulipSite: cfg.ZulipSite}
	lk.Register(mux)
	bot = zulip.NewBot(zc, st, botID, lk.HandleDM)
	go bot.Run(ctx)
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("listening", "addr", srv.Addr)

	// ponytail: only the web server so far; poller/sender/bot loops join this
	// shutdown via ctx when they exist.
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func routes(st *store.Store) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := st.DB.PingContext(r.Context()); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	return mux
}
