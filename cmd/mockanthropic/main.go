// Command mockanthropic is a mock Anthropic Messages API upstream. It streams
// paced, seeded lorem ipsum and applies faults set through POST /_faults.
//
// The fault API has no authentication. Keep the default loopback address, or
// expose the mock only on a private network.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/NimbleEngineer21/tokenwarden/internal/mock"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mockanthropic:", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:8081", "listen address")
	ttft := flag.Duration("ttft", 300*time.Millisecond, "time to first token")
	tps := flag.Float64("tps", 40, "tokens per second; 0 sends at full speed")
	jitter := flag.Float64("jitter", 0.2, "fraction from 0 to 1 that varies each delta wait")
	flag.Parse()
	switch {
	case *ttft < 0:
		return errors.New("-ttft must not be negative")
	case *tps < 0:
		return errors.New("-tps must not be negative")
	case *jitter < 0 || *jitter > 1:
		return errors.New("-jitter must be from 0 to 1")
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mock.New(mock.Config{TTFT: *ttft, TokensPerSecond: *tps, Jitter: *jitter, Logger: log}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: a long-reply stream stays open for minutes.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.InfoContext(ctx, "listening", "addr", *addr)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		// Open streams outlived the grace period; cut them.
		return errors.Join(err, srv.Close())
	}
	return nil
}
