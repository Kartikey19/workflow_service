// Package obs holds the process metrics (05 §6). Labels are low-cardinality by
// construction: never a run ID, node ID or tenant name.
package obs

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

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

// RunDuration is observed when a run reaches a terminal status. The workflow
// label is the workflow name: bounded by the number of workflows, not runs.
var RunDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name: "wf_run_duration_seconds", Help: "Run duration from start to terminal status.",
	Buckets: []float64{.1, .5, 1, 5, 15, 60, 300, 900, 3600, 21600, 86400, 604800},
}, []string{"workflow", "status"})

func init() { Registry.MustRegister(RunDuration) }

// Querier is the database surface the engine gauges read at scrape time.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

var (
	gaugeOnce sync.Once
	gaugeDB   atomic.Pointer[Querier]
)

// RegisterEngineGauges exposes wf_ready_backlog and wf_runs_active{status},
// read from PostgreSQL at scrape time (05 §6). Safe to call more than once;
// the latest database wins.
func RegisterEngineGauges(db Querier) {
	gaugeDB.Store(&db)
	gaugeOnce.Do(func() { Registry.MustRegister(engineGauges{}) })
}

var (
	readyBacklogDesc = prometheus.NewDesc("wf_ready_backlog", "Ready node executions whose run_after has passed; rising means too few workers.", nil, nil)
	runsActiveDesc   = prometheus.NewDesc("wf_runs_active", "Runs not yet terminal, by status.", []string{"status"}, nil)
)

type engineGauges struct{}

func (engineGauges) Describe(ch chan<- *prometheus.Desc) {
	ch <- readyBacklogDesc
	ch <- runsActiveDesc
}

func (engineGauges) Collect(ch chan<- prometheus.Metric) {
	pointer := gaugeDB.Load()
	if pointer == nil {
		return
	}
	db := *pointer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var backlog int64
	if err := db.QueryRow(ctx, `select count(*) from node_executions where status = 'ready' and run_after <= now()`).Scan(&backlog); err == nil {
		ch <- prometheus.MustNewConstMetric(readyBacklogDesc, prometheus.GaugeValue, float64(backlog))
	}
	counts := map[string]float64{"pending": 0, "running": 0, "waiting": 0}
	rows, err := db.Query(ctx, `select status, count(*) from runs where status in ('pending', 'running', 'waiting') group by status`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int64
		if rows.Scan(&status, &count) == nil {
			counts[status] = float64(count)
		}
	}
	if rows.Err() != nil {
		return
	}
	for status, count := range counts {
		ch <- prometheus.MustNewConstMetric(runsActiveDesc, prometheus.GaugeValue, count, status)
	}
}
