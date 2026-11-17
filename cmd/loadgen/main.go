// Command loadgen sends concurrent Messages API requests to one or more
// gateways and reports the outcome of each request, the token counts, and
// the latency.
//
// The gateway key comes from the environment variable TW_GATEWAY_KEY, never
// from a flag, so that it does not show in the process list or in shell
// history. See docs/loadgen.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// errUsage means that the command line was wrong and the error is already
// printed.
var errUsage = errors.New("usage error")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	switch {
	case errors.Is(err, errUsage):
		os.Exit(2)
	case err != nil:
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

// config is the parsed command line and environment.
type config struct {
	urls        []string
	key         string
	concurrency int
	requests    int
	duration    time.Duration
	timeout     time.Duration
	model       string
	maxTokens   int64
	promptWords int
	stream      bool
	json        bool
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	cfg, err := parseConfig(args, getenv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConnsPerHost: cfg.concurrency,
		ForceAttemptHTTP2:   true,
	}}
	start := time.Now()
	results := generate(ctx, cfg, client)
	rep := summarize(results, time.Since(start))
	if cfg.json {
		return json.NewEncoder(stdout).Encode(rep)
	}
	return rep.writeText(stdout)
}

func parseConfig(args []string, getenv func(string) string, stderr io.Writer) (config, error) {
	cfg := config{key: getenv("TW_GATEWAY_KEY")}
	var urls string
	fs := flagSet(&cfg, &urls)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return config{}, err
		}
		return config{}, errUsage
	}
	var err error
	cfg.urls, err = parseURLs(urls)
	switch {
	case err != nil:
	case fs.NArg() > 0:
		err = fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case cfg.key == "":
		err = errors.New("TW_GATEWAY_KEY is not set")
	case cfg.concurrency < 1:
		err = errors.New("-concurrency must be at least 1")
	case cfg.requests < 0 || cfg.duration < 0:
		err = errors.New("-requests and -duration must not be negative")
	case cfg.requests == 0 && cfg.duration == 0:
		err = errors.New("set -requests or -duration")
	case cfg.timeout <= 0:
		err = errors.New("-timeout must be positive")
	case cfg.maxTokens < 1:
		err = errors.New("-max-tokens must be at least 1")
	case cfg.promptWords < 0:
		err = errors.New("-prompt-words must not be negative")
	}
	if err != nil {
		fmt.Fprintln(stderr, "loadgen:", err)
		return config{}, errUsage
	}
	return cfg, nil
}

// flagSet returns the flags of loadgen, bound to cfg and urls.
func flagSet(cfg *config, urls *string) *flag.FlagSet {
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.StringVar(urls, "url", "http://127.0.0.1:8080", "gateway base URL; a comma-separated list sends to each in turn")
	fs.IntVar(&cfg.concurrency, "concurrency", 10, "number of requests in flight")
	fs.IntVar(&cfg.requests, "requests", 100, "number of requests to send; 0 = no limit (needs -duration)")
	fs.DurationVar(&cfg.duration, "duration", 0, "stop starting requests after this time; 0 = no limit")
	fs.DurationVar(&cfg.timeout, "timeout", 5*time.Minute, "time limit of one request")
	fs.StringVar(&cfg.model, "model", "claude-sonnet-4-5", "model of each request")
	fs.Int64Var(&cfg.maxTokens, "max-tokens", 256, "max_tokens of each request")
	fs.IntVar(&cfg.promptWords, "prompt-words", 50, "number of words in each prompt")
	fs.BoolVar(&cfg.stream, "stream", true, "send streaming requests")
	fs.BoolVar(&cfg.json, "json", false, "print the report as one JSON object")
	return fs
}

// parseURLs splits a comma-separated list of gateway base URLs.
func parseURLs(list string) ([]string, error) {
	var urls []string
	for s := range strings.SplitSeq(list, ",") {
		u, err := url.Parse(strings.TrimSpace(s))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("-url %q: want an http or https URL", s)
		}
		urls = append(urls, strings.TrimSuffix(u.String(), "/"))
	}
	return urls, nil
}
