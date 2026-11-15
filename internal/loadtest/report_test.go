package loadtest

import (
	"encoding/json"
	"testing"
	"time"
)

func us(n int) time.Duration { return time.Duration(n) * time.Microsecond }

func TestMedianRange(t *testing.T) {
	tests := []struct {
		in          []int
		med, lo, hi int
	}{
		{[]int{5}, 5, 5, 5},
		{[]int{9, 1, 5}, 5, 1, 9},
		// Even count: the lower middle value, so the median is always a
		// value some round really had.
		{[]int{4, 1, 3, 2}, 2, 1, 4},
		{[]int{42, 85, 43, 42, 43}, 43, 42, 85},
	}
	for _, tt := range tests {
		med, lo, hi := MedianRange(tt.in)
		if med != tt.med || lo != tt.lo || hi != tt.hi {
			t.Errorf("MedianRange(%v) = %d, %d, %d; want %d, %d, %d", tt.in, med, lo, hi, tt.med, tt.lo, tt.hi)
		}
	}
	if med, lo, hi := MedianRange([]float64(nil)); med != 0 || lo != 0 || hi != 0 {
		t.Errorf("MedianRange(nil) = %v, %v, %v; want zeros", med, lo, hi)
	}
}

func TestMedianRangeLeavesInputAlone(t *testing.T) {
	in := []int{3, 1, 2}
	MedianRange(in)
	if in[0] != 3 || in[1] != 1 || in[2] != 2 {
		t.Errorf("input reordered to %v", in)
	}
}

func testReport() Report {
	round := func(directP50, gatewayP50, directP99, gatewayP99 int, rates ...float64) Round {
		r := Round{
			Direct:  StdioRun{Summary: Summary{P50: us(directP50), P99: us(directP99)}},
			Gateway: StdioRun{Summary: Summary{P50: us(gatewayP50), P99: us(gatewayP99)}},
		}
		for i, rate := range rates {
			r.HTTP = append(r.HTTP, Throughput{Clients: []int{1, 32}[i], PerSecond: rate, Latency: Summary{P99: us(100 * (i + 1))}, Calls: 1000, Refused: i * 2})
		}
		return r
	}
	return Report{Rounds: []Round{
		round(22, 64, 35, 190, 9700, 16000),
		round(21, 107, 60, 280, 9500, 15000), // a slow round
		round(22, 65, 34, 183, 9800, 18000),
	}}
}

func TestReportAddedLatency(t *testing.T) {
	p50, p99 := testReport().AddedLatency()
	if p50 != (Spread[time.Duration]{Median: us(43), Min: us(42), Max: us(86)}) {
		t.Errorf("p50 = %+v, want median 43µs, 42µs to 86µs", p50)
	}
	if p99 != (Spread[time.Duration]{Median: us(155), Min: us(149), Max: us(220)}) {
		t.Errorf("p99 = %+v, want median 155µs, 149µs to 220µs", p99)
	}
}

func TestReportThroughputByClients(t *testing.T) {
	got := testReport().ThroughputByClients()
	if len(got) != 2 {
		t.Fatalf("got %d rows, want one per client count", len(got))
	}
	if got[0].Clients != 1 || got[0].PerSecond != (Spread[float64]{Median: 9700, Min: 9500, Max: 9800}) {
		t.Errorf("1 client = %+v", got[0])
	}
	if got[0].Calls != 3000 || got[0].Refused != 0 || got[1].Calls != 3000 || got[1].Refused != 6 {
		t.Errorf("totals: 1 client %d calls %d refused, 32 clients %d calls %d refused; want 3000/0 and 3000/6", got[0].Calls, got[0].Refused, got[1].Calls, got[1].Refused)
	}
	if got[1].Clients != 32 || got[1].PerSecond != (Spread[float64]{Median: 16000, Min: 15000, Max: 18000}) || got[1].P99.Median != us(200) {
		t.Errorf("32 clients = %+v", got[1])
	}
}

func TestStdioRunSamplesAreSavedAsNanoseconds(t *testing.T) {
	data, err := json.Marshal(StdioRun{Samples: []time.Duration{1500, 2 * time.Microsecond}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"summary":{"n":0,"min_ns":0,"mean_ns":0,"p50_ns":0,"p90_ns":0,"p99_ns":0,"max_ns":0},"samples_ns":[1500,2000]}`; string(data) != want {
		t.Errorf("JSON = %s\nwant %s", data, want)
	}
}
