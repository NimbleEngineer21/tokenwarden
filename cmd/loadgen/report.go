package main

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

// report is the summary of a run. Its JSON form is the -json output.
type report struct {
	Requests       int            `json:"requests"`
	ElapsedSeconds float64        `json:"elapsed_seconds"`
	Outcomes       map[string]int `json:"outcomes"`
	// InputTokens counts input, cache creation, and cache read tokens, the
	// same sum that the gateway charges. Token counts come from the usage
	// that each reply reports, so a stream cut before its message_delta
	// adds input tokens only.
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// TTFT covers successful streams that sent content; Latency covers all
	// successful requests.
	TTFT    quantiles `json:"ttft_ms"`
	Latency quantiles `json:"latency_ms"`
}

// quantiles summarizes a set of durations in milliseconds.
type quantiles struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

func summarize(results []result, elapsed time.Duration) report {
	rep := report{
		Requests:       len(results),
		ElapsedSeconds: elapsed.Seconds(),
		Outcomes:       map[string]int{},
	}
	var ttft, latency []time.Duration
	for _, r := range results {
		rep.Outcomes[r.outcome]++
		u := r.usage
		rep.InputTokens += u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
		rep.OutputTokens += u.OutputTokens
		if r.outcome != outcomeOK {
			continue
		}
		latency = append(latency, r.latency)
		if r.ttft > 0 {
			ttft = append(ttft, r.ttft)
		}
	}
	rep.TTFT = quantilesOf(ttft)
	rep.Latency = quantilesOf(latency)
	return rep
}

// quantilesOf returns nearest-rank quantiles of ds. It sorts ds.
func quantilesOf(ds []time.Duration) quantiles {
	if len(ds) == 0 {
		return quantiles{}
	}
	slices.Sort(ds)
	at := func(pct int) float64 {
		// Nearest rank, in integers so that no float rounding moves the
		// rank: the smallest value with at least pct percent of the set at
		// or below it.
		rank := (pct*len(ds) + 99) / 100
		return ms(ds[max(rank, 1)-1])
	}
	return quantiles{N: len(ds), P50: at(50), P95: at(95), P99: at(99), Max: ms(ds[len(ds)-1])}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func (r report) writeText(w io.Writer) error {
	var outcomes []string
	for _, k := range slices.Sorted(maps.Keys(r.Outcomes)) {
		outcomes = append(outcomes, fmt.Sprintf("%s=%d", k, r.Outcomes[k]))
	}
	var rate, outRate float64
	if r.ElapsedSeconds > 0 {
		rate = float64(r.Requests) / r.ElapsedSeconds
		outRate = float64(r.OutputTokens) / r.ElapsedSeconds
	}
	_, err := fmt.Fprintf(w, "requests  %d in %.1fs (%.1f/s)\n"+
		"outcomes  %s\n"+
		"tokens    input=%d output=%d (%.0f output/s)\n"+
		"ttft      %s\n"+
		"latency   %s\n",
		r.Requests, r.ElapsedSeconds, rate,
		strings.Join(outcomes, " "),
		r.InputTokens, r.OutputTokens, outRate,
		r.TTFT, r.Latency)
	return err
}

// String formats q for the text report.
func (q quantiles) String() string {
	return fmt.Sprintf("n=%d p50=%.0fms p95=%.0fms p99=%.0fms max=%.0fms", q.N, q.P50, q.P95, q.P99, q.Max)
}
