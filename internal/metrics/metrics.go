// Package metrics exposes what the gateway does as Prometheus metrics.
//
// Every label value comes from a bounded set, because Prometheus keeps a
// time series per distinct value: agent names come from the config, tool
// names only if an upstream listed them (anything else is "other"), and the
// rest are fixed words. An agent inventing tool names can't grow memory.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/audit"
)

const namespace = "mcpgw"

// otherTool and noAgent stand in for label values that aren't bounded.
const (
	otherTool = "other"
	noAgent   = "none"
)

// maxConcurrentScrapes and scrapeTimeout keep a misbehaving scraper from
// using up the gateway.
const (
	maxConcurrentScrapes = 4
	scrapeTimeout        = 10 * time.Second
)

// Buckets in seconds. Tool calls range from a millisecond (a local
// lookup) to a minute (a long job), and the gateway's own overhead is
// sub-millisecond, so the low end is finer.
var (
	upstreamBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	gatewayBuckets  = []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
)

// Metrics holds the gateway's metrics and their registry. It is an
// audit.Observer.
type Metrics struct {
	reg        *prometheus.Registry
	knownTool  func(string) bool
	knownAgent func(string) bool

	calls    *prometheus.CounterVec
	denials  *prometheus.CounterVec
	duration *prometheus.HistogramVec
	upstream *prometheus.HistogramVec
	refused  prometheus.Counter
}

// New registers the metrics. knownTool says whether a tool name is real
// (router.KnownTool); knownAgent whether an agent name is in the config.
func New(knownTool, knownAgent func(string) bool) *Metrics {
	m := &Metrics{
		reg:        prometheus.NewRegistry(),
		knownTool:  knownTool,
		knownAgent: knownAgent,
		calls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "tool_calls_total",
			Help: "Tool calls by agent, tool, decision (allowed, denied, rate_limited), and how they ended.",
		}, []string{"agent", "tool", "decision", "status"}),
		denials: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "tool_call_denials_total",
			Help: "Refused tool calls by reason: tool (not allowed or unknown), arguments, or rate_limited.",
		}, []string{"agent", "tool", "reason"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "tool_call_duration_seconds",
			Help:    "Total time a tool call spent in the gateway, upstream included.",
			Buckets: gatewayBuckets,
		}, []string{"decision"}),
		upstream: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "upstream_latency_seconds",
			Help:    "Time forwarded tool calls spent upstream.",
			Buckets: upstreamBuckets,
		}, []string{"tool"}),
		refused: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "audit_refused_total",
			Help: "Tool calls refused because the audit log had no room. Any increase needs a look.",
		}),
	}
	m.reg.MustRegister(m.calls, m.denials, m.duration, m.upstream, m.refused,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// WatchAuditQueue reports how full the audit queue is at each scrape.
func (m *Metrics) WatchAuditQueue(used, capacity func() int) {
	m.reg.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: namespace, Name: "audit_queue_slots_used",
			Help: "Audit queue slots taken: tool calls in flight plus entries not yet written.",
		}, func() float64 { return float64(used()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: namespace, Name: "audit_queue_slots",
			Help: "Audit queue size (-audit-queue).",
		}, func() float64 { return float64(capacity()) }),
	)
}

// CallDone counts one finished tool call.
func (m *Metrics) CallDone(e audit.Entry, total time.Duration) {
	agent, tool := m.agentLabel(e.Agent), m.toolLabel(e.Tool)
	m.calls.WithLabelValues(agent, tool, string(e.Decision), string(e.Status)).Inc()
	m.duration.WithLabelValues(string(e.Decision)).Observe(total.Seconds())
	if reason := denialReason(e); reason != "" {
		m.denials.WithLabelValues(agent, tool, reason).Inc()
	}
	if e.Latency > 0 {
		m.upstream.WithLabelValues(tool).Observe(e.Latency.Seconds())
	}
}

// CallRefused counts a call the audit log had no room for.
func (m *Metrics) CallRefused() {
	m.refused.Inc()
}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		MaxRequestsInFlight: maxConcurrentScrapes,
		Timeout:             scrapeTimeout,
	})
}

func (m *Metrics) toolLabel(tool string) string {
	if m.knownTool(tool) {
		return tool
	}
	return otherTool
}

func (m *Metrics) agentLabel(agent string) string {
	if m.knownAgent(agent) {
		return agent
	}
	return noAgent
}

// denialReason sorts refusals into a few kinds. The policy answers a tool
// it won't allow with a JSON-RPC error and bad arguments with an isError
// result, so the status tells the two apart.
func denialReason(e audit.Entry) string {
	switch {
	case e.Decision == audit.RateLimited:
		return "rate_limited"
	case e.Decision != audit.Denied:
		return ""
	case e.Status == audit.StatusToolError:
		return "arguments"
	default:
		return "tool"
	}
}
