// Package loadtest measures the gateway: how much time it adds to a call,
// and how many calls it handles at once. cmd/loadtest runs it.
package loadtest

import (
	"slices"
	"time"
)

// Summary describes a set of measured call times.
type Summary struct {
	N    int           `json:"n"`
	Min  time.Duration `json:"min_ns"`
	Mean time.Duration `json:"mean_ns"`
	P50  time.Duration `json:"p50_ns"`
	P90  time.Duration `json:"p90_ns"`
	P99  time.Duration `json:"p99_ns"`
	Max  time.Duration `json:"max_ns"`
}

// Summarize returns the summary of samples without changing them. An empty
// slice gives a zero Summary.
//
// Percentiles use the nearest-rank method: p is the smallest sample that at
// least p% of samples are at or below. It always returns a measured value,
// never one made up by averaging two neighbours.
func Summarize(samples []time.Duration) Summary {
	if len(samples) == 0 {
		return Summary{}
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)

	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	return Summary{
		N:    len(sorted),
		Min:  sorted[0],
		Mean: total / time.Duration(len(sorted)),
		P50:  percentile(sorted, 50),
		P90:  percentile(sorted, 90),
		P99:  percentile(sorted, 99),
		Max:  sorted[len(sorted)-1],
	}
}

// percentile returns the nearest-rank p-th percentile of sorted, which must
// not be empty.
func percentile(sorted []time.Duration, p int) time.Duration {
	// rank = ceil(p/100 * n), in integers so 99% of 100 is exactly 99.
	rank := (p*len(sorted) + 99) / 100
	return sorted[max(rank, 1)-1]
}
