// Package obs holds the process metrics (05 §6). Labels are low-cardinality by
// construction: never a run ID, node ID or tenant name.
package obs

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var Registry = prometheus.NewRegistry()

var (
	NodesClaimed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "wf_nodes_claimed_total", Help: "Node executions claimed by this worker.",
	})
	NodeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "wf_node_duration_seconds", Help: "Executor duration per attempt.",
		Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300},
	}, []string{"node_type", "status"})
	NodeAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wf_node_attempts_total", Help: "Finished node attempts by outcome and error code.",
	}, []string{"node_type", "status", "code"})
	LeaseExpired = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "wf_lease_expired_total", Help: "Leases reclaimed by the sweeper; nonzero means workers died or LeaseTTL is too short.",
	})
	LeaseLost = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "wf_lease_lost_total", Help: "Results discarded by fenced writes.",
	})
	ClaimBatch = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "wf_claim_batch_size", Help: "Rows returned per claim.",
		Buckets: []float64{0, 1, 2, 5, 10, 20, 50},
	})
	RunsFinished = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wf_runs_finished_total", Help: "Runs reaching a terminal status.",
	}, []string{"status"})
	RunsStarted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wf_runs_started_total", Help: "Runs created by trigger.",
	}, []string{"trigger"})
	SweeperActions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wf_sweeper_actions_total", Help: "Sweeper corrections by action.",
	}, []string{"action"})
	Signals = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wf_signal_total", Help: "Signal deliveries by result.",
	}, []string{"result"})
	HTTPRequests = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "wf_http_request_duration_seconds", Help: "API request latency by route pattern.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "code"})
	AgentTokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wf_agent_tokens_total", Help: "LLM tokens by provider and direction.",
	}, []string{"provider", "direction"})
	AgentCost = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wf_agent_cost_usd_total", Help: "Estimated LLM spend by provider.",
	}, []string{"provider"})
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		NodesClaimed, NodeDuration, NodeAttempts, LeaseExpired, LeaseLost, ClaimBatch,
		RunsFinished, RunsStarted, SweeperActions, Signals, HTTPRequests, AgentTokens, AgentCost,
	)
}

// Handler serves the Prometheus exposition format.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{})
}

// RegisterGauge adds a gauge whose value is read at scrape time.
func RegisterGauge(name, help string, read func() float64) {
	Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, read))
}
