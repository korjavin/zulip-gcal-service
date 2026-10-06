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
	"github.com/korjavin/zulip-gcal-service/internal/lifecycle"
	"github.com/korjavin/zulip-gcal-service/internal/link"
	"github.com/korjavin/zulip-gcal-service/internal/poller"
	"github.com/korjavin/zulip-gcal-service/internal/sender"
	"github.com/korjavin/zulip-gcal-service/internal/settings"
	"github.com/korjavin/zulip-gcal-service/internal/store"
	"github.com/korjavin/zulip-gcal-service/internal/web"
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
	ops := lifecycle.New(cfg, st, zc)
	ops.Register(mux, au.Account)
	pl := poller.New(st, ops.TokenSource, cfg.PollInterval)
	ops.Poll = pl.Trigger
	go pl.Run(ctx)
	senderDone := make(chan struct{}) // joined at shutdown: its last outcome write lands before the DB closes
	go func() { sender.New(st, zc).Run(ctx); close(senderDone) }()
	var bot *zulip.Bot
	lk := &link.Linker{St: st, Zulip: zc, Account: au.Account, BotHealthy: func() bool { return bot.Healthy() },
		BotID: botID, PublicURL: cfg.PublicURL, ZulipSite: cfg.ZulipSite, Poll: pl.Trigger}
	lk.Register(mux)
	(&web.Site{St: st, Zulip: zc, Account: au.Account, PollInterval: cfg.PollInterval,
		Resume: func(ctx context.Context, id string) error { _, err := ops.Resume(ctx, id); return err }}).Register(mux)
	(&settings.Page{St: st, Zulip: zc, Account: au.Account, CSRF: au.CSRFToken, Tokens: ops.TokenSource, Poll: pl.Trigger,
		Pause: func(ctx context.Context, id string) error { _, err := ops.Pause(ctx, id); return err }}).Register(mux)
	bot = zulip.NewBot(zc, st, botID, lk.HandleDM)
	go bot.Run(ctx)
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("listening", "addr", srv.Addr)

	// The poller, sender and bot loops stop via ctx.
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
	<-senderDone
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
