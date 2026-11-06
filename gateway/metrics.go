package gateway

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/NimbleEngineer21/tokenwarden/audit"
)

// metrics are the Prometheus collectors of one gateway.
type metrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	active   prometheus.Gauge
	tokens   *prometheus.CounterVec
}

func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tokenwarden_requests_total",
			Help: "Messages API requests by outcome and stream flag.",
		}, []string{"outcome", "stream"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "tokenwarden_request_duration_seconds",
			Help: "Time from request to the end of the reply.",
			// 50 ms to about 7 minutes: a long stream stays open for minutes.
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 14),
		}, []string{"stream"}),
		active: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tokenwarden_streams_active",
			Help: "Streams that the gateway is relaying now.",
		}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tokenwarden_tokens_total",
			Help: "Tokens that the upstream reported, by team and kind.",
		}, []string{"team", "kind"}),
	}
	if reg == nil {
		return m, nil
	}
	var errs []error
	for _, c := range []prometheus.Collector{m.requests, m.duration, m.active, m.tokens} {
		errs = append(errs, reg.Register(c))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *metrics) observe(ev *audit.Event) {
	stream := "false"
	if ev.Stream {
		stream = "true"
	}
	m.requests.WithLabelValues(string(ev.Outcome), stream).Inc()
	m.duration.WithLabelValues(stream).Observe(ev.Duration.Seconds())
	if ev.Team == "" {
		return
	}
	for kind, n := range map[string]int64{
		"input":          ev.Usage.InputTokens,
		"output":         ev.Usage.OutputTokens,
		"cache_creation": ev.Usage.CacheCreationInputTokens,
		"cache_read":     ev.Usage.CacheReadInputTokens,
	} {
		if n > 0 {
			m.tokens.WithLabelValues(ev.Team, kind).Add(float64(n))
		}
	}
}
