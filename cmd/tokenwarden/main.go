// Command tokenwarden is the reference gateway. It proxies the Anthropic
// Messages API, authenticates clients with gateway keys from a key file
// or from Postgres, asks a CEL policy file about each request, enforces
// each team's token budget, and writes one audit log line per request.
//
// The upstream URL and key come from the environment variables
// TW_UPSTREAM_URL and TW_UPSTREAM_KEY, and the database from
// TW_DATABASE_URL, never from flags, so that no secret shows in the process
// list or in shell history. Prometheus metrics are served on a separate
// listener, never on the proxy port. See docs/tokenwarden.md.
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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/NimbleEngineer21/tokenwarden/audit"
	"github.com/NimbleEngineer21/tokenwarden/auth"
	authpg "github.com/NimbleEngineer21/tokenwarden/auth/postgres"
	"github.com/NimbleEngineer21/tokenwarden/gateway"
	"github.com/NimbleEngineer21/tokenwarden/limit"
	"github.com/NimbleEngineer21/tokenwarden/limit/postgres"
	"github.com/NimbleEngineer21/tokenwarden/policy"
	"github.com/NimbleEngineer21/tokenwarden/policy/cel"
)

// shutdownGrace is how long open requests can run after a stop signal.
// Streams that are still open after it are cut.
const shutdownGrace = 10 * time.Second

// dbStartTimeout bounds the database connect and migration at start.
const dbStartTimeout = 30 * time.Second

// errUsage means that the command line was wrong and the usage text is
// already printed.
var errUsage = errors.New("usage error")

// serveHTTP starts one server. Tests replace it to start Serve late, which
// the scheduler does only rarely.
var serveHTTP = (*http.Server).Serve

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// After the first signal, restore the default handling, so that a second
	// signal stops the process during the grace period.
	context.AfterFunc(ctx, stop)
	err := run(ctx, os.Args[1:], os.Getenv, os.Stderr)
	stop()
	switch {
	case errors.Is(err, errUsage):
		os.Exit(2)
	case err != nil:
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
	databaseURL string
	noBudgets   bool
	dbKeys      bool
	policyPath  string
	noPolicy    bool
	policyPoll  time.Duration
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
	// SIGHUP reloads the policy. Catch it before anything else, so that a
	// SIGHUP never kills the gateway, which is the default for Go.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	proxy, metrics, reload, closeDB, err := handlers(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer closeDB()
	hupCtx, stopHUP := context.WithCancel(ctx)
	defer stopHUP()
	go reloadOnSignal(hupCtx, hup, reload)
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
	return serve(ctx, log, []served{{"proxy", proxySrv, proxyLn}, {"metrics", metricsSrv, metricsLn}})
}

func parseConfig(args []string, getenv func(string) string, stderr io.Writer) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("tokenwarden", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.addr, "addr", "127.0.0.1:8080", "proxy listen address")
	fs.StringVar(&cfg.metricsAddr, "metrics-addr", "127.0.0.1:9090", "metrics listen address; must use a port other than -addr")
	fs.StringVar(&cfg.keysPath, "keys", "", "JSON key file of SHA-256 gateway key hashes; give this or -db-keys")
	fs.BoolVar(&cfg.dbKeys, "db-keys", false, "read gateway keys from the database of TW_DATABASE_URL; give this or -keys")
	fs.BoolVar(&cfg.noBudgets, "no-budgets", false, "enforce no token budgets; required when TW_DATABASE_URL is not set")
	fs.StringVar(&cfg.policyPath, "policy", "", "CEL policy file; give this or -no-policy")
	fs.BoolVar(&cfg.noPolicy, "no-policy", false, "allow every authenticated request; give this or -policy")
	fs.DurationVar(&cfg.policyPoll, "policy-poll", 10*time.Second, "how often to check the policy file for changes; 0 checks only on SIGHUP")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return config{}, err
		}
		return config{}, errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return config{}, errUsage
	}
	pollSet := false
	fs.Visit(func(f *flag.Flag) { pollSet = pollSet || f.Name == "policy-poll" })
	cfg.upstreamURL = getenv("TW_UPSTREAM_URL")
	cfg.upstreamKey = getenv("TW_UPSTREAM_KEY")
	cfg.databaseURL = getenv("TW_DATABASE_URL")
	switch {
	case cfg.upstreamURL == "":
		return config{}, errors.New("TW_UPSTREAM_URL is not set")
	case cfg.upstreamKey == "":
		return config{}, errors.New("TW_UPSTREAM_KEY is not set")
	case cfg.keysPath == "" && !cfg.dbKeys:
		return config{}, errors.New("-keys or -db-keys is required")
	case cfg.keysPath != "" && cfg.dbKeys:
		return config{}, errors.New("-keys and -db-keys are both set; use one")
	case cfg.databaseURL == "" && cfg.dbKeys:
		return config{}, errors.New("TW_DATABASE_URL is not set; -db-keys reads the keys from it")
	// A gateway without budgets must be a choice, never a missing variable.
	case cfg.databaseURL == "" && !cfg.noBudgets:
		return config{}, errors.New("TW_DATABASE_URL is not set; set it, or pass -no-budgets to run without token budgets")
	// With -db-keys the database holds the keys, so -no-budgets with it is
	// a gateway with keys in Postgres and no budgets.
	case cfg.databaseURL != "" && cfg.noBudgets && !cfg.dbKeys:
		return config{}, errors.New("-no-budgets and TW_DATABASE_URL are both set; use one")
	// A gateway without a policy must be a choice, the same as budgets.
	case cfg.policyPath == "" && !cfg.noPolicy:
		return config{}, errors.New("-policy or -no-policy is required")
	case cfg.policyPath != "" && cfg.noPolicy:
		return config{}, errors.New("-policy and -no-policy are both set; use one")
	case pollSet && cfg.policyPath == "":
		return config{}, errors.New("-policy-poll needs -policy")
	case cfg.policyPoll < 0:
		return config{}, errors.New("-policy-poll must not be negative")
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

func loadKeys(path string, log *slog.Logger) (*auth.Static, error) {
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
	log.Info("loaded gateway keys", "count", s.Len())
	return s, nil
}

// openDB connects to the database, migrates it, and returns the pool. The
// error never holds the database URL, which can hold a password.
func openDB(ctx context.Context, dbURL string) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, errors.New("TW_DATABASE_URL is not a valid Postgres connection string")
	}
	ctx, cancel := context.WithTimeout(ctx, dbStartTimeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: %w", err)
	}
	return pool, nil
}

// dbKeys returns the key store of pool. It logs the number of active keys.
// Zero is a warning, not an error: in a new deployment the keys are added
// after the first gateway creates the schema.
func dbKeys(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (auth.Authenticator, error) {
	a := authpg.New(pool)
	ctx, cancel := context.WithTimeout(ctx, dbStartTimeout)
	defer cancel()
	n, err := a.Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	if n == 0 {
		log.WarnContext(ctx, "the database holds no active gateway keys; every request gets 401 until one is added")
	}
	log.InfoContext(ctx, "reading gateway keys from the database", "count", n)
	return a, nil
}

// policyMetrics are the collectors of the policy file reloads.
type policyMetrics struct {
	reloads     *prometheus.CounterVec
	lastSuccess prometheus.Gauge
}

func newPolicyMetrics(reg prometheus.Registerer) (*policyMetrics, error) {
	m := &policyMetrics{
		reloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tokenwarden_policy_reloads_total",
			Help: "Loads of the policy file by result: ok, or error when the gateway kept its rules.",
		}, []string{"result"}),
		lastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tokenwarden_policy_last_reload_success_timestamp_seconds",
			Help: "Unix time of the last load of the policy file that worked.",
		}),
	}
	if err := errors.Join(reg.Register(m.reloads), reg.Register(m.lastSuccess)); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *policyMetrics) observe(_ string, err error) {
	if err != nil {
		m.reloads.WithLabelValues("error").Inc()
		return
	}
	m.reloads.WithLabelValues("ok").Inc()
	m.lastSuccess.SetToCurrentTime()
}

// loadPolicy loads the policy file of cfg and returns it with a func that
// reloads it. With -no-policy it returns a nil engine and a func that only
// logs.
func loadPolicy(ctx context.Context, cfg config, reg prometheus.Registerer, log *slog.Logger) (*cel.Engine, func(), error) {
	if cfg.noPolicy {
		log.WarnContext(ctx, "the policy is off (-no-policy): every authenticated request is allowed")
		return nil, func() { log.InfoContext(ctx, "SIGHUP: no policy to reload") }, nil
	}
	m, err := newPolicyMetrics(reg)
	if err != nil {
		return nil, nil, err
	}
	// The error starts with "policy: <path>", so it needs no prefix.
	e, err := cel.Load(cfg.policyPath, cel.WithLogger(log), cel.WithReloadFunc(m.observe))
	if err != nil {
		return nil, nil, err
	}
	// The engine logs a reload that works; a failed one is logged here.
	reload := func() {
		if err := e.Reload(); err != nil {
			log.ErrorContext(ctx, "policy reload failed; the old rules stay", "error", err)
		}
	}
	return e, reload, nil
}

// reloadOnSignal calls reload for each signal on sigs until ctx is done.
func reloadOnSignal(ctx context.Context, sigs <-chan os.Signal, reload func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sigs:
			reload()
		}
	}
}

// startLeases starts the limiter's lease loop and returns a func that stops
// it and then closes the database. The loop outlives ctx, the signal
// context, so that it renews the leases of the requests that run during the
// shutdown grace period.
func startLeases(ctx context.Context, l *postgres.Limiter, closePool func()) func() {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
		closePool()
	}
}

// handlers returns the proxy handler, the metrics handler, a func that
// reloads the policy, and a func that closes the database. Each call has its
// own metrics registry. With a policy file and a positive -policy-poll, it
// also watches the file until ctx is done.
func handlers(ctx context.Context, cfg config, log *slog.Logger) (proxy, metrics http.Handler, reload, closeDB func(), err error) {
	reg := prometheus.NewRegistry()
	if err := errors.Join(
		reg.Register(collectors.NewGoCollector()),
		reg.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})),
	); err != nil {
		return nil, nil, nil, nil, err
	}
	// A bad key file or policy file fails before any database connect.
	var keys auth.Authenticator
	if !cfg.dbKeys {
		if keys, err = loadKeys(cfg.keysPath, log); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	engine, reload, err := loadPolicy(ctx, cfg, reg, log)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	// A nil interface, not a nil *cel.Engine: the gateway checks the
	// interface for nil.
	var pol policy.Policy
	if engine != nil {
		pol = engine
	}
	// One pool serves the keys and the budgets.
	var pool *pgxpool.Pool
	closeDB = func() {}
	if cfg.dbKeys || !cfg.noBudgets {
		if pool, err = openDB(ctx, cfg.databaseURL); err != nil {
			return nil, nil, nil, nil, err
		}
		closeDB = pool.Close
	}
	if cfg.dbKeys {
		if keys, err = dbKeys(ctx, pool, log); err != nil {
			closeDB()
			return nil, nil, nil, nil, err
		}
	}
	// A nil interface, not a nil *postgres.Limiter: the gateway checks the
	// interface for nil.
	var limiter limit.Limiter
	if cfg.noBudgets {
		log.WarnContext(ctx, "token budgets are off (-no-budgets)")
	} else {
		reaped := prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tokenwarden_reaped_reservations_total",
			Help: "Reservations reaped after their lease ended.",
		})
		reapedTokens := prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tokenwarden_reaped_tokens_total",
			Help: "Tokens that reaped reservations held, charged to their windows.",
		})
		if err := errors.Join(reg.Register(reaped), reg.Register(reapedTokens)); err != nil {
			closeDB()
			return nil, nil, nil, nil, err
		}
		l := postgres.New(pool,
			postgres.WithLogger(log),
			postgres.WithReapFunc(func(n int, tokens int64) {
				reaped.Add(float64(n))
				reapedTokens.Add(float64(tokens))
			}))
		limiter, closeDB = l, startLeases(ctx, l, pool.Close)
	}
	gw, err := gateway.New(gateway.Config{
		UpstreamURL:   cfg.upstreamURL,
		UpstreamKey:   cfg.upstreamKey,
		Authenticator: keys,
		Logger:        log,
		Recorder:      audit.NewSlog(log),
		Registerer:    reg,
		Limiter:       limiter,
		Policy:        pol,
	})
	if err != nil {
		closeDB()
		return nil, nil, nil, nil, err
	}

	pm := http.NewServeMux()
	pm.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	pm.Handle("/", gw)

	mm := http.NewServeMux()
	mm.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	if engine != nil && cfg.policyPoll > 0 {
		go engine.Watch(ctx, cfg.policyPoll)
	}
	return pm, mm, reload, closeDB, nil
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

// served is a named server and the listener that it serves on.
type served struct {
	name string
	srv  *http.Server
	ln   net.Listener
}

// serve runs each server until ctx is done or one server fails. Then it
// shuts all of them down, and after [shutdownGrace] it cuts the open
// requests. Cutting requests at the end of the grace period is normal, not
// an error. serve returns only after every listener is closed.
func serve(ctx context.Context, log *slog.Logger, all []served) error {
	errc := make(chan error, len(all))
	for _, s := range all {
		go func() { errc <- serveHTTP(s.srv, s.ln) }()
	}
	var serveErr error
	pending := len(all)
	select {
	case serveErr = <-errc:
		pending--
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
				if errors.Is(err, context.DeadlineExceeded) {
					log.WarnContext(sctx, "shutdown grace period ended; open requests cut", "server", s.name, "grace", shutdownGrace.String())
					err = nil
				}
				errs[i] = errors.Join(err, s.srv.Close())
			}
			done <- i
		}()
	}
	for range all {
		<-done
	}
	// Shutdown closes only the listeners that Serve already tracks. A Serve
	// call that starts after Shutdown closes its own listener and returns, so
	// wait for each one; otherwise a listener can outlive serve.
	for range pending {
		if err := <-errc; serveErr == nil || errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = err
		}
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(append(errs, serveErr)...)
}
