package loadtest

import (
	"math/rand/v2"
	"testing"
	"time"
)

func ms(n ...int) []time.Duration {
	out := make([]time.Duration, len(n))
	for i, v := range n {
		out[i] = time.Duration(v) * time.Millisecond
	}
	return out
}

func TestSummarize(t *testing.T) {
	tests := []struct {
		name string
		in   []time.Duration
		want Summary
	}{
		{
			name: "one sample is every percentile",
			in:   ms(7),
			want: Summary{N: 1, Min: 7 * time.Millisecond, Mean: 7 * time.Millisecond, P50: 7 * time.Millisecond, P90: 7 * time.Millisecond, P99: 7 * time.Millisecond, Max: 7 * time.Millisecond},
		},
		{
			// Nearest rank: p50 of 1..10 is the 5th value, p90 the 9th, p99 the 10th.
			name: "ten samples in any order",
			in:   ms(10, 3, 1, 8, 5, 2, 9, 4, 7, 6),
			want: Summary{N: 10, Min: 1 * time.Millisecond, Mean: 5500 * time.Microsecond, P50: 5 * time.Millisecond, P90: 9 * time.Millisecond, P99: 10 * time.Millisecond, Max: 10 * time.Millisecond},
		},
		{
			// p99 of 100 values is the 99th, so one slow outlier stays out of it.
			name: "one outlier in a hundred",
			in:   append(ms(make([]int, 99)...), time.Second),
			want: Summary{N: 100, Mean: 10 * time.Millisecond, Max: time.Second},
		},
		{
			name: "no samples",
			in:   nil,
			want: Summary{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Summarize(tt.in); got != tt.want {
				t.Errorf("Summarize() = %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// With 1000 values 1..1000 ms, each percentile is exactly that many ms.
func TestSummarizeLargeShuffled(t *testing.T) {
	in := make([]time.Duration, 1000)
	for i := range in {
		in[i] = time.Duration(i+1) * time.Millisecond
	}
	rand.Shuffle(len(in), func(i, j int) { in[i], in[j] = in[j], in[i] })
	got := Summarize(in)
	if got.P50 != 500*time.Millisecond || got.P90 != 900*time.Millisecond || got.P99 != 990*time.Millisecond {
		t.Errorf("p50 %v, p90 %v, p99 %v; want 500ms, 900ms, 990ms", got.P50, got.P90, got.P99)
	}
}

func TestSummarizeLeavesInputAlone(t *testing.T) {
	in := ms(3, 1, 2)
	Summarize(in)
	if in[0] != 3*time.Millisecond || in[1] != time.Millisecond || in[2] != 2*time.Millisecond {
		t.Errorf("input reordered to %v", in)
	}
}
