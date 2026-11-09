// Command tokenwarden is the reference gateway. It proxies the Anthropic
// Messages API, authenticates clients with gateway keys from a key file, and
// writes one audit log line per request.
//
// The upstream URL and key come from the environment variables
// TW_UPSTREAM_URL and TW_UPSTREAM_KEY, never from flags, so that the key does
// not show in the process list or in shell history. Prometheus metrics are
// served on a separate listener, never on the proxy port. See
// docs/tokenwarden.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/NimbleEngineer21/tokenwarden/audit"
	"github.com/NimbleEngineer21/tokenwarden/auth"
	"github.com/NimbleEngineer21/tokenwarden/gateway"
)

// shutdownGrace is how long open requests can run after a stop signal.
// Streams that are still open after it are cut.
const shutdownGrace = 10 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Getenv, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tokenwarden:", err)
		os.Exit(1)
	}
}

// config is the parsed command line and environment.
type config struct {
	addr        string
	metricsAddr string
	keysPath    string
	upstreamURL string
	upstreamKey string
}

func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) error {
	cfg, err := parseConfig(args, getenv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	proxy, metrics, err := handlers(cfg, log)
	if err != nil {
		return err
	}
	var lc net.ListenConfig
	proxyLn, err := lc.Listen(ctx, "tcp", cfg.addr)
	if err != nil {
		return err
	}
	metricsLn, err := lc.Listen(ctx, "tcp", cfg.metricsAddr)
	if err != nil {
		return errors.Join(err, proxyLn.Close())
	}
	log.InfoContext(ctx, "listening", "addr", proxyLn.Addr().String(), "metrics_addr", metricsLn.Addr().String())
	proxySrv, metricsSrv := servers(proxy, metrics)
	return serve(ctx, []served{{proxySrv, proxyLn}, {metricsSrv, metricsLn}})
}

func parseConfig(args []string, getenv func(string) string, stderr io.Writer) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("tokenwarden", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.addr, "addr", "127.0.0.1:8080", "proxy listen address")
	fs.StringVar(&cfg.metricsAddr, "metrics-addr", "127.0.0.1:9090", "metrics listen address; must use a port other than -addr")
	fs.StringVar(&cfg.keysPath, "keys", "", "JSON key file of SHA-256 gateway key hashes (required)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	cfg.upstreamURL = getenv("TW_UPSTREAM_URL")
	cfg.upstreamKey = getenv("TW_UPSTREAM_KEY")
	switch {
	case cfg.upstreamURL == "":
		return config{}, errors.New("TW_UPSTREAM_URL is not set")
	case cfg.upstreamKey == "":
		return config{}, errors.New("TW_UPSTREAM_KEY is not set")
	case cfg.keysPath == "":
		return config{}, errors.New("-keys is required")
	}
	proxyPort, err := port(cfg.addr)
	if err != nil {
		return config{}, fmt.Errorf("-addr: %w", err)
	}
	metricsPort, err := port(cfg.metricsAddr)
	if err != nil {
		return config{}, fmt.Errorf("-metrics-addr: %w", err)
	}
	// Compare ports, not addresses: with SO_REUSEADDR, BSD and macOS let a
	// specific host bind beside a wildcard on one port, and the metrics
	// listener would then take proxy traffic. Port 0 picks a free port.
	if proxyPort != 0 && proxyPort == metricsPort {
		return config{}, errors.New("-metrics-addr must use a port other than -addr")
	}
	return cfg, nil
}

// port returns the port of a host:port address.
func port(addr string) (int, error) {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 0 || n > 65535 {
		return 0, fmt.Errorf("port %q must be a number from 0 to 65535", p)
	}
	return n, nil
}

func loadKeys(path string) (*auth.Static, error) {
	f, err := os.Open(path) //nolint:gosec // The operator names the key file; it is not request input.
	if err != nil {
		return nil, fmt.Errorf("-keys: %w", err)
	}
	defer f.Close()
	s, err := auth.LoadStatic(f)
	if err != nil {
		return nil, fmt.Errorf("-keys %s: %w", path, err)
	}
	// A gateway with no keys refuses every request; that is a config mistake.
	if s.Len() == 0 {
		return nil, fmt.Errorf("-keys %s: the file holds no keys", path)
	}
	return s, nil
}

// handlers returns the proxy handler and the metrics handler. Each call has
// its own metrics registry.
func handlers(cfg config, log *slog.Logger) (proxy, metrics http.Handler, err error) {
	keys, err := loadKeys(cfg.keysPath)
	if err != nil {
		return nil, nil, err
	}
	reg := prometheus.NewRegistry()
	if err := errors.Join(
		reg.Register(collectors.NewGoCollector()),
		reg.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})),
	); err != nil {
		return nil, nil, err
	}
	gw, err := gateway.New(gateway.Config{
		UpstreamURL:   cfg.upstreamURL,
		UpstreamKey:   cfg.upstreamKey,
		Authenticator: keys,
		Logger:        log,
		Recorder:      audit.NewSlog(log),
		Registerer:    reg,
	})
	if err != nil {
		return nil, nil, err
	}
	log.Info("loaded gateway keys", "count", keys.Len())

	pm := http.NewServeMux()
	pm.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	pm.Handle("/", gw)

	mm := http.NewServeMux()
	mm.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	return pm, mm, nil
}

// servers returns the HTTP servers for the proxy and metrics handlers.
func servers(proxy, metrics http.Handler) (proxySrv, metricsSrv *http.Server) {
	proxySrv = &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: a stream stays open for minutes.
	}
	metricsSrv = &http.Server{
		Handler:           metrics,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	return proxySrv, metricsSrv
}

// served is a server and the listener that it serves on.
type served struct {
	srv *http.Server
	ln  net.Listener
}

// serve runs each server until ctx is done or one server fails. Then it
// shuts all of them down, and after [shutdownGrace] it cuts the open
// requests.
func serve(ctx context.Context, all []served) error {
	errc := make(chan error, len(all))
	for _, s := range all {
		go func() { errc <- s.srv.Serve(s.ln) }()
	}
	var serveErr error
	select {
	case serveErr = <-errc:
	case <-ctx.Done():
	}
	// ctx can be done already; the grace period needs a live context.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	errs := make([]error, len(all))
	done := make(chan int, len(all))
	for i, s := range all {
		go func() {
			if err := s.srv.Shutdown(sctx); err != nil {
				errs[i] = errors.Join(err, s.srv.Close())
			}
			done <- i
		}()
	}
	for range all {
		<-done
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(append(errs, serveErr)...)
}
