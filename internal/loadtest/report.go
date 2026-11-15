package loadtest

import (
	"cmp"
	"slices"
	"time"
)

// Report is everything one benchmark run measured, saved as JSON so the
// numbers in the README can be traced back to a run.
type Report struct {
	Started  time.Time `json:"started"`
	Machine  Machine   `json:"machine"`
	Settings Settings  `json:"settings"`
	Rounds   []Round   `json:"rounds"`
}

// Machine says where and with what code the run happened.
type Machine struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	CPU       string `json:"cpu"`
	CPUs      int    `json:"cpus"`
	GoVersion string `json:"go_version"`
	Commit    string `json:"commit"`   // "unknown" when not built from git (go run)
	Modified  bool   `json:"modified"` // uncommitted changes when built
}

// Settings are the options the run used.
type Settings struct {
	Calls      int           `json:"calls"`
	Warmup     int           `json:"warmup"`
	Clients    []int         `json:"clients"`
	Window     time.Duration `json:"window_ns"`
	HTTPWarmup time.Duration `json:"http_warmup_ns"`
	Policy     string        `json:"policy"`
}

// Round is one pass over every measurement.
type Round struct {
	StdioOrder []string     `json:"stdio_order"` // which stdio target ran first
	Direct     StdioRun     `json:"direct"`
	Gateway    StdioRun     `json:"gateway"`
	HTTP       []Throughput `json:"http"`
}

// StdioRun is one stdio target's calls: the summary and every call's time.
type StdioRun struct {
	Summary Summary         `json:"summary"`
	Samples []time.Duration `json:"samples_ns"`
}

// Spread is the median of a value across rounds and its lowest and highest.
type Spread[T cmp.Ordered] struct {
	Median T `json:"median"`
	Min    T `json:"min"`
	Max    T `json:"max"`
}

// MedianRange returns the median, lowest, and highest of vals without
// changing them. With an even count the median is the lower of the two
// middle values, so it is always a value that was measured. Empty input
// gives zeros.
func MedianRange[T cmp.Ordered](vals []T) (median, lo, hi T) {
	if len(vals) == 0 {
		return median, lo, hi
	}
	sorted := slices.Clone(vals)
	slices.Sort(sorted)
	return sorted[(len(sorted)-1)/2], sorted[0], sorted[len(sorted)-1]
}

func spreadOf[T cmp.Ordered](vals []T) Spread[T] {
	median, lo, hi := MedianRange(vals)
	return Spread[T]{Median: median, Min: lo, Max: hi}
}

// AddedLatency is how much the gateway added at p50 and p99, per round
// (gateway minus direct), spread across rounds.
func (r Report) AddedLatency() (p50, p99 Spread[time.Duration]) {
	var added50, added99 []time.Duration
	for _, round := range r.Rounds {
		added50 = append(added50, round.Gateway.Summary.P50-round.Direct.Summary.P50)
		added99 = append(added99, round.Gateway.Summary.P99-round.Direct.Summary.P99)
	}
	return spreadOf(added50), spreadOf(added99)
}

// ClientsSpread is one HTTP client count's results across rounds.
type ClientsSpread struct {
	Clients   int
	PerSecond Spread[float64]
	P99       Spread[time.Duration]
	Calls     int // answered calls, all rounds together
	Refused   int // refused calls, all rounds together
}

// ThroughputByClients spreads each client count's calls per second and p99
// across rounds, in the order the counts were run.
func (r Report) ThroughputByClients() []ClientsSpread {
	var order []int
	rates := map[int][]float64{}
	p99s := map[int][]time.Duration{}
	calls, refused := map[int]int{}, map[int]int{}
	for _, round := range r.Rounds {
		for _, t := range round.HTTP {
			if _, seen := rates[t.Clients]; !seen {
				order = append(order, t.Clients)
			}
			rates[t.Clients] = append(rates[t.Clients], t.PerSecond)
			p99s[t.Clients] = append(p99s[t.Clients], t.Latency.P99)
			calls[t.Clients] += t.Calls
			refused[t.Clients] += t.Refused
		}
	}
	out := make([]ClientsSpread, 0, len(order))
	for _, c := range order {
		out = append(out, ClientsSpread{Clients: c, PerSecond: spreadOf(rates[c]), P99: spreadOf(p99s[c]), Calls: calls[c], Refused: refused[c]})
	}
	return out
}
