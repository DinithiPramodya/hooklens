// Command hooklens is the hooklens server: the capture endpoint, the JSON API,
// the tunnel hub, and (from Phase 2) the embedded web UI, in one binary.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/config"
	"github.com/DinithiPramodya/hooklens/internal/server"
	"github.com/DinithiPramodya/hooklens/internal/store"
	"github.com/DinithiPramodya/hooklens/internal/sweeper"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// main does nothing but call run and translate its error into an exit code.
	// Calling os.Exit directly from deeper code would skip every deferred
	// function on the way out; keeping the exit in one place means everything
	// else can just return an error.
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// `forward` is dispatched BEFORE config.Load and before any signal
	// handling that mentions the database, because it is the client half of
	// the product: it runs on a developer's laptop, talks to a remote server,
	// and has no business requiring a DATABASE_URL to be sensible. It gets its
	// own signal context so ctrl-c stops it cleanly.
	if args := os.Args[1:]; len(args) > 0 && args[0] == "forward" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runForward(ctx, args[1:])
	}

	// `version` is dispatched here, above config.Load, for the same reason
	// `forward` is: asking what version a binary is must not require a
	// database URL. It also has to work before anything else does, because
	// it is the first thing a packager runs -- the Homebrew formula's test
	// block is literally `hooklens version`, so a version command that needs
	// configuration is a failing `brew install`.
	if args := os.Args[1:]; len(args) > 0 && (args[0] == "version" || args[0] == "--version" || args[0] == "-v") {
		fmt.Println("hooklens", version)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg.Env)

	// NotifyContext gives a context that is cancelled on SIGINT or SIGTERM.
	// SIGTERM is the one that matters in production: it is what Docker, Fly and
	// Kubernetes send first, followed by SIGKILL after a grace period. Handling
	// it is the difference between finishing the webhook captures currently in
	// flight and dropping them.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Subcommand dispatch. Hand-rolled rather than a CLI library: there are
	// three subcommands, and the default -- no arguments at all -- has to stay
	// `serve` so the container ENTRYPOINT needs no arguments.
	if args := os.Args[1:]; len(args) > 0 {
		switch args[0] {
		case "migrate":
			return runMigrate(ctx, log, cfg.DatabaseURL, args[1:])
		case "serve":
			// Explicit form of the default.
		default:
			return fmt.Errorf("unknown command %q (want serve, migrate, forward or version)", args[0])
		}
	}

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		// Refuse to start rather than boot and fail on the first webhook. The
		// liveness endpoint deliberately does not check the database (see
		// handleHealth), so a process that starts without one would report
		// healthy while dropping every capture.
		return err
	}
	defer st.Close()

	// A derived, explicitly cancellable context. serve() returns for two
	// reasons: the signal context was cancelled (normal shutdown), or
	// ListenAndServe failed (port taken). In the SECOND case the signal
	// context is still live, so waiting on the sweeper here without
	// cancelling first would block forever -- a startup error turning into
	// a hang is a worse failure than the error itself.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sw := sweeper.New(st, log, cfg.SweepInterval, sweeper.DefaultBatch)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sw.Run(runCtx)
	}()

	serveErr := serve(runCtx, cfg, log, st, sw)

	// Stop the sweeper and WAIT for it before returning -- the deferred
	// st.Close() runs the moment we do, and closing the pool out from under
	// an in-flight DELETE is how you get "conn closed" noise on every clean
	// shutdown.
	cancel()
	wg.Wait()

	return serveErr
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger, st *store.Store, sw *sweeper.Sweeper) error {
	log.Info("starting", "version", version, "env", cfg.Env, "addr", cfg.Addr, "base_domain", cfg.BaseDomain)

	handler := server.New(ctx, cfg, log, st)

	// The rate limiters keep one bucket per key, so something has to forget
	// the idle ones -- otherwise the map grows with every distinct client,
	// which is a leak driven by exactly the traffic a limiter exists to
	// handle. Reusing the sweeper's timer rather than starting a second
	// goroutine: one thing to shut down, and it already recovers from panics.
	sw.OnTick(func() {
		if c, p := handler.EvictLimiters(); c+p > 0 {
			log.Debug("evicted idle rate limiters", "create", c, "capture", p)
		}
		if n := handler.EvictTouched(); n > 0 {
			log.Debug("forgot last-seen bookkeeping for idle inboxes", "inboxes", n)
		}
		if n := handler.EvictEndpointCache(); n > 0 {
			log.Debug("dropped expired inbox cache entries", "entries", n)
		}
	})

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: handler,

		// ReadHeaderTimeout, not ReadTimeout. ReadTimeout caps the time to read
		// headers AND body, and a legitimate provider on a slow link may take a
		// while over a large payload. Headers, though, should arrive promptly --
		// a client that opens a connection and dribbles headers forever is the
		// Slowloris attack, and this is the line that closes it.
		ReadHeaderTimeout: 10 * time.Second,

		// Bounded explicitly rather than inheriting Go's 1MB default. Headers
		// travel inside a tunnel frame alongside a base64 body, and the frame
		// limit has to cover both -- a megabyte of headers would blow past it
		// and close the connection. 64KB is far more than any real webhook
		// sends and keeps the arithmetic in internal/tunnel honest.
		MaxHeaderBytes: 64 << 10,

		// A response we cannot write in 30s is not going to get better.
		WriteTimeout: 30 * time.Second,

		// How long an idle keep-alive connection is held open. Providers reuse
		// connections across deliveries, so this is worth having.
		IdleTimeout: 120 * time.Second,

		// Route the server's own errors through our logger instead of the
		// standard one, so everything is in the same format in production.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	// ListenAndServe blocks, so it runs in its own goroutine and reports back
	// over a buffered channel. Buffered so the goroutine can send and exit even
	// if nobody is left to receive -- an unbuffered send here would leak it.
	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		// The server stopped on its own: the port is taken, or the socket died.
		// ErrServerClosed only appears after a Shutdown, which cannot have
		// happened on this branch, so any error here is real.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil

	case <-ctx.Done():
		log.Info("shutdown signal received, draining")

		// Fresh context, not ctx: ctx is already cancelled -- that is why we are
		// here -- and passing it to Shutdown would abort the drain instantly.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		// Shutdown stops accepting new connections and waits for in-flight
		// handlers to return. If the deadline passes first it gives up and
		// returns an error, leaving those connections to be cut.
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		log.Info("stopped cleanly")
		return nil
	}
}

// newLogger returns a text logger in development and JSON in production.
//
// JSON because in production nothing reads these with human eyes -- they go to a
// log aggregator that indexes fields. Text in development because you do.
func newLogger(env string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if env == "prod" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
