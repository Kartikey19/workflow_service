package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"runtime/debug"
	"strings"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/config"
	"github.com/kartikey/workflow_service/internal/executor"
	"github.com/kartikey/workflow_service/internal/expr"
	"github.com/kartikey/workflow_service/internal/flow"
	"github.com/kartikey/workflow_service/internal/ids"
	"github.com/kartikey/workflow_service/internal/obs"
	"github.com/kartikey/workflow_service/internal/spec"
	"github.com/kartikey/workflow_service/internal/store"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"go.uber.org/zap"
)

// DefaultMaxPayloadBytes caps node input and output rows (01 §2.8).
const DefaultMaxPayloadBytes = 256 << 10

// SecretSource resolves a tenant's vault for ${secrets.NAME}. Values are passed
// to executors only and never persisted or logged.
type SecretSource interface {
	Secrets(ctx context.Context, tenantID pgtype.UUID) (map[string]string, error)
}

type Dispatcher struct {
	cfg        config.WorkerConfig
	flow       *flow.Flow
	store      *gen.Queries
	registry   *executor.Registry
	secrets    SecretSource
	logger     *zap.Logger
	maxPayload int
}

// NewDispatcher shares flow's spec cache and expression environment across all
// node executions this worker runs. secrets may be nil.
func NewDispatcher(cfg config.WorkerConfig, f *flow.Flow, registry *executor.Registry, secrets SecretSource, logger *zap.Logger) *Dispatcher {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Dispatcher{
		cfg: cfg, flow: f, store: f.Q, registry: registry, secrets: secrets,
		logger: logger, maxPayload: DefaultMaxPayloadBytes,
	}
}

// outcome is one attempt's terminal or parked write.
type outcome struct {
	status  string // succeeded | failed | skipped | waiting
	code    string // error code for metrics; "" on success
	output  []byte
	err     []byte
	metrics []byte
	retryAt time.Time // non-zero: keep the failed row and enqueue attempt+1
	gotoID  *string   // branch: the chosen successor
	park    *park     // sleep / wait_event
}

type park struct {
	until time.Time
	wait  *executor.WaitSpec
}

func (d *Dispatcher) Run(parent context.Context, ne gen.NodeExecution) {
	obs.NodesClaimed.Inc()
	ctx, stopHeartbeat, err := d.startHeartbeat(parent, ne)
	if err != nil {
		d.logLeaseError(parent, ne, err)
		return
	}
	defer stopHeartbeat()
	log := d.logger.With(
		zap.String("run_id", ids.String(ne.RunID)), zap.String("node_id", ne.NodeID),
		zap.Int32("attempt", ne.Attempt), zap.String("node_execution_id", ids.String(ne.ID)),
		zap.String("worker_id", d.cfg.ID),
	)

	// 1. Load the run and its compiled spec. Database errors are transient:
	// returning lets the lease expire and the node be re-delivered (same attempt).
	run, err := d.store.GetRun(ctx, gen.GetRunParams{ID: ne.RunID, TenantID: ne.TenantID})
	if err != nil {
		log.Error("load run for node execution", zap.Error(err))
		return
	}
	if flow.IsTerminalRun(run.Status) {
		d.cancelStale(parent, ne, nil)
		return
	}
	compiled, err := d.flow.Specs.Get(ctx, d.store, run.WorkflowVersionID, run.TenantID)
	if errors.Is(err, flow.ErrSpecInvalid) {
		d.failPermanent(ctx, ne, nil, "E_SPEC_DECODE", err)
		return
	}
	if err != nil {
		log.Error("load workflow spec", zap.Error(err))
		return
	}

	// From here on a failure is deterministic and fails the node permanently.
	node, ok := compiled.Spec.Node(ne.NodeID)
	if !ok {
		d.failPermanent(ctx, ne, compiled, "E_NODE_NOT_IN_SPEC", errors.New("node missing from published workflow spec"))
		return
	}
	programs := compiled.Programs.Nodes[node.ID]
	ex, exists := d.registry.Get(node.Type)
	if !exists {
		d.failPermanent(ctx, ne, compiled, "E_EXECUTOR_NOT_REGISTERED", fmt.Errorf("node type %q is not available on this worker", node.Type))
		return
	}

	// 2. Bindings: run input and every finished node, in one query.
	bindings, err := flow.Bindings(ctx, d.store, run, ne.BranchKey)
	if err != nil {
		log.Error("build expression bindings", zap.Error(err))
		return
	}

	// 3. `if` false writes a real skipped row so the skip cascades (02 §2).
	if programs.If != nil {
		ok, err := programs.If.EvalBool(ctx, bindings)
		if err != nil {
			d.failPermanent(ctx, ne, compiled, "E_EXPR_EVAL", fmt.Errorf("evaluate if: %w", err))
			return
		}
		if !ok {
			d.persist(parent, ctx, ne, compiled, outcome{status: "skipped"})
			return
		}
	}

	// 4. Resolve config. Secrets are resolved for the executor only; the stored
	// input is resolved again with every secret replaced by "***" (03 §4.2 (a)).
	stored, resolvedCfg, err := d.resolveConfig(ctx, run, node, programs.Config, bindings)
	if err != nil {
		d.failPermanent(ctx, ne, compiled, "E_EXPR_EVAL", err)
		return
	}
	if len(stored) > d.maxPayload {
		d.failPermanent(ctx, ne, compiled, "input_too_large", fmt.Errorf("resolved input is %d bytes, over the %d byte limit", len(stored), d.maxPayload))
		return
	}
	if err := d.markStarted(ctx, ne, stored); err != nil {
		if errors.Is(err, errLeaseLost) {
			d.logLeaseError(ctx, ne, err)
			return
		}
		log.Error("persist node input", zap.Error(err))
		return
	}

	// 5. Execute outside any transaction, bounded by the node timeout.
	req := executor.Request{
		RunID:           uuid.UUID(ne.RunID.Bytes),
		NodeExecutionID: uuid.UUID(ne.ID.Bytes),
		TenantID:        uuid.UUID(ne.TenantID.Bytes),
		NodeID:          ne.NodeID,
		BranchKey:       ne.BranchKey,
		Attempt:         int(ne.Attempt),
		Config:          resolvedCfg,
		IdempotencyKey:  ne.IdempotencyKey,
		Emit:            func(context.Context, string, any) {},
	}
	attemptCtx, cancelAttempt := attemptContext(ctx, node)
	defer cancelAttempt()
	if deadline, ok := attemptCtx.Deadline(); ok {
		req.Deadline = deadline
	}
	started := time.Now()
	res, execErr := d.execute(attemptCtx, ex, ne, req)
	elapsed := time.Since(started)
	metrics := encodeMetrics(res.Metrics, elapsed)

	// 6. Classify, then persist and advance in one transaction.
	var o outcome
	switch {
	case execErr != nil:
		o = failedOutcome(ne, node, execErr, metrics)
	case len(res.Output) > 0 && !json.Valid(res.Output):
		o = permanentOutcome("invalid_output", errors.New("executor returned invalid JSON output"), metrics)
	case len(res.Output) > d.maxPayload:
		o = permanentOutcome("output_too_large", fmt.Errorf("output is %d bytes, over the %d byte limit", len(res.Output), d.maxPayload), metrics)
	case res.Control != nil && res.Control.Sleep != nil:
		o = outcome{status: "waiting", output: res.Output, metrics: metrics, park: &park{until: *res.Control.Sleep}}
	case res.Control != nil && res.Control.Wait != nil:
		o = outcome{status: "waiting", output: res.Output, metrics: metrics, park: &park{until: res.Control.Wait.Expires, wait: res.Control.Wait}}
	case res.Control != nil:
		target := res.Control.Goto
		o = outcome{status: "succeeded", output: res.Output, metrics: metrics, gotoID: &target}
	default:
		o = outcome{status: "succeeded", output: res.Output, metrics: metrics}
	}
	if len(o.output) == 0 && o.status == "succeeded" {
		o.output = []byte(`{}`)
	}
	obs.NodeDuration.WithLabelValues(node.Type, o.status).Observe(elapsed.Seconds())
	d.persist(parent, ctx, ne, compiled, o)
}

func (d *Dispatcher) resolveConfig(ctx context.Context, run gen.Run, node spec.NodeSpec, template *expr.Template, bindings expr.Bindings) (stored, resolved []byte, err error) {
	if template == nil {
		return []byte(`{}`), []byte(`{}`), nil
	}
	usesSecrets := false
	if raw, err := json.Marshal(node.Config); err == nil {
		usesSecrets = strings.Contains(string(raw), "secrets")
	}
	if usesSecrets && d.secrets != nil {
		values, err := d.secrets.Secrets(ctx, run.TenantID)
		if err != nil {
			return nil, nil, fmt.Errorf("load vault secrets: %w", err)
		}
		real := bindings
		real.Secrets = values
		value, err := template.Resolve(ctx, real)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve config: %w", err)
		}
		if resolved, err = json.Marshal(value); err != nil {
			return nil, nil, fmt.Errorf("encode resolved config: %w", err)
		}
		redacted := bindings
		redacted.Secrets = make(map[string]string, len(values))
		for name := range values {
			redacted.Secrets[name] = "***"
		}
		value, err = template.Resolve(ctx, redacted)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve config: %w", err)
		}
		stored, err = json.Marshal(value)
		return stored, resolved, err
	}
	value, err := template.Resolve(ctx, bindings)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve config: %w", err)
	}
	resolved, err = json.Marshal(value)
	if err != nil {
		return nil, nil, fmt.Errorf("encode resolved config: %w", err)
	}
	return resolved, resolved, nil
}

// markStarted records the resolved input and the node.started event.
func (d *Dispatcher) markStarted(ctx context.Context, ne gen.NodeExecution, input []byte) error {
	return store.InTx(ctx, d.flow.Pool, func(tx pgx.Tx) error {
		q := d.store.WithTx(tx)
		run, err := flow.LockRun(ctx, q, ne.RunID)
		if err != nil {
			return err
		}
		rows, err := q.SetNodeInput(ctx, gen.SetNodeInputParams{ID: ne.ID, WorkerID: d.leaseOwner(), Input: input})
		if err != nil {
			return fmt.Errorf("set node input: %w", err)
		}
		if rows == 0 {
			return errLeaseLost
		}
		if err := flow.AppendEvent(ctx, q, run, "node.started", ne.NodeID, ne.Attempt, map[string]any{"worker_id": d.cfg.ID}); err != nil {
			return err
		}
		return q.NotifyRunEvents(ctx, ids.String(ne.RunID))
	})
}

// persist writes an attempt's outcome and advances the run in one transaction
// (03 §4.4). parent outlives the heartbeat; ctx is cancelled on lease loss.
// A zero-row fenced write means another worker owns the node: the result is
// discarded and logged, never forced or retried.
func (d *Dispatcher) persist(parent, ctx context.Context, ne gen.NodeExecution, compiled *spec.Compiled, o outcome) {
	if err := ctx.Err(); err != nil {
		// The heartbeat stopped: the lease was lost, the run ended, or the worker
		// is shutting down. A cancelled run's node is marked cancelled now.
		obs.LeaseLost.Inc()
		d.logger.Warn("discard node result", zap.Error(err),
			zap.String("node_execution_id", ids.String(ne.ID)), zap.String("status", o.status))
		d.cancelStale(parent, ne, o.metrics)
		return
	}

	err := store.InTx(ctx, d.flow.Pool, func(tx pgx.Tx) error {
		q := d.store.WithTx(tx)
		run, err := flow.LockRun(ctx, q, ne.RunID)
		if err != nil {
			return err
		}
		if flow.IsTerminalRun(run.Status) {
			_, err := q.CancelRunningNode(ctx, gen.CancelRunningNodeParams{ID: ne.ID, WorkerID: d.leaseOwner(), Metrics: o.metrics})
			return err
		}
		if o.park != nil && o.park.wait != nil {
			_, err := q.PeekLiveWait(ctx, gen.PeekLiveWaitParams{TenantID: ne.TenantID, WaitKey: o.park.wait.Key})
			if err == nil {
				// Another live run waits on this key: fail loudly now rather than
				// misroute a signal later (03 §7.3).
				o = permanentOutcome("E_WAIT_KEY_CONFLICT", fmt.Errorf("another run is already waiting on key %q", o.park.wait.Key), o.metrics)
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("check wait key: %w", err)
			}
		}

		if o.park != nil {
			rows, err := q.ParkNode(ctx, gen.ParkNodeParams{
				ID: ne.ID, WorkerID: d.leaseOwner(), Output: o.output, Metrics: o.metrics,
				RunAfter: pgtype.Timestamptz{Time: o.park.until, Valid: true},
			})
			if err != nil {
				return fmt.Errorf("park node: %w", err)
			}
			if rows == 0 {
				return errLeaseLost
			}
			payload := map[string]any{"until": o.park.until.UTC().Format(time.RFC3339)}
			if o.park.wait != nil {
				if err := q.InsertWait(ctx, gen.InsertWaitParams{
					ID: ids.New(), TenantID: ne.TenantID, RunID: ne.RunID, NodeExecutionID: ne.ID,
					WaitKey: o.park.wait.Key, ExpiresAt: pgtype.Timestamptz{Time: o.park.wait.Expires, Valid: true},
					Schema: waitSchema(o.park.wait.Schema),
				}); err != nil {
					return fmt.Errorf("register wait: %w", err)
				}
				payload["key"] = o.park.wait.Key
			}
			if err := flow.AppendEvent(ctx, q, run, "node.waiting", ne.NodeID, ne.Attempt, payload); err != nil {
				return err
			}
			if err := d.flow.Recompute(ctx, q, run, compiled); err != nil {
				return err
			}
			return q.NotifyRunEvents(ctx, ids.String(ne.RunID))
		}

		rows, err := q.CompleteNode(ctx, gen.CompleteNodeParams{
			ID: ne.ID, WorkerID: d.leaseOwner(), Status: o.status,
			Output: o.output, Error: o.err, Metrics: o.metrics,
		})
		if err != nil {
			return fmt.Errorf("complete node execution: %w", err)
		}
		if rows == 0 {
			return errLeaseLost
		}
		payload := map[string]any{}
		if o.code != "" {
			payload["code"] = o.code
		}
		if len(o.output) > 0 {
			payload["output_bytes"] = len(o.output)
		}
		if err := flow.AppendEvent(ctx, q, run, "node."+o.status, ne.NodeID, ne.Attempt, payload); err != nil {
			return err
		}

		if !o.retryAt.IsZero() && d.flow.MaxNodesPerRun > 0 {
			count, err := q.CountRunNodeExecutions(ctx, ne.RunID)
			if err != nil {
				return fmt.Errorf("count run node executions: %w", err)
			}
			if count >= int64(d.flow.MaxNodesPerRun) {
				// Poison-node protection (03 §6.4): the budget wins over retries.
				return d.flow.FailRun(ctx, q, run, "E_RUN_BUDGET", fmt.Sprintf("run exceeded %d node executions", d.flow.MaxNodesPerRun))
			}
		}
		if !o.retryAt.IsZero() {
			if _, err := q.EnqueueRetry(ctx, gen.EnqueueRetryParams{
				ID: ids.New(), RunID: ne.RunID, TenantID: ne.TenantID, NodeID: ne.NodeID,
				NodeType: ne.NodeType, BranchKey: ne.BranchKey, Attempt: ne.Attempt + 1,
				IdempotencyKey: ne.IdempotencyKey, // identical across attempts (I5)
				RunAfter:       pgtype.Timestamptz{Time: o.retryAt, Valid: true},
			}); err != nil {
				return fmt.Errorf("enqueue node retry: %w", err)
			}
			if err := flow.AppendEvent(ctx, q, run, "node.retrying", ne.NodeID, ne.Attempt+1, map[string]any{
				"delay_ms": time.Until(o.retryAt).Milliseconds(),
			}); err != nil {
				return err
			}
			return q.NotifyRunEvents(ctx, ids.String(ne.RunID))
		}

		if compiled == nil {
			// The spec itself could not be loaded; nothing can advance this run.
			if err := d.flow.FailRun(ctx, q, run, "E_SPEC_DECODE", "published workflow spec could not be loaded"); err != nil {
				return err
			}
		} else if err := d.flow.Advance(ctx, q, run, compiled, flow.Completed{
			NodeID: ne.NodeID, BranchKey: ne.BranchKey, Status: o.status, Goto: o.gotoID,
		}); err != nil {
			return err
		}
		return q.NotifyRunEvents(ctx, ids.String(ne.RunID))
	})

	switch {
	case errors.Is(err, errLeaseLost):
		obs.LeaseLost.Inc()
		d.logLeaseError(ctx, ne, err)
	case err != nil:
		d.logger.Error("persist node outcome", zap.Error(err),
			zap.String("node_execution_id", ids.String(ne.ID)), zap.String("status", o.status))
	default:
		obs.NodeAttempts.WithLabelValues(ne.NodeType, o.status, o.code).Inc()
	}
}

// cancelStale marks this worker's running node cancelled when its run already
// ended (cancelled, failed, or past its deadline). Fenced by lease owner.
func (d *Dispatcher) cancelStale(parent context.Context, ne gen.NodeExecution, metrics []byte) {
	if parent.Err() != nil {
		return // shutdown: the lease is released or expires and the node is re-delivered
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	run, err := d.store.GetRun(ctx, gen.GetRunParams{ID: ne.RunID, TenantID: ne.TenantID})
	if err != nil || !flow.IsTerminalRun(run.Status) {
		return
	}
	if _, err := d.store.CancelRunningNode(ctx, gen.CancelRunningNodeParams{ID: ne.ID, WorkerID: d.leaseOwner(), Metrics: metrics}); err != nil {
		d.logger.Warn("mark stale node cancelled", zap.Error(err), zap.String("node_execution_id", ids.String(ne.ID)))
	}
}

// failPermanent fails the attempt without retry, for deterministic errors found
// before or instead of executing the node.
func (d *Dispatcher) failPermanent(ctx context.Context, ne gen.NodeExecution, compiled *spec.Compiled, code string, err error) {
	d.logger.Warn("fail node execution permanently", zap.Error(err), zap.String("code", code),
		zap.String("node_execution_id", ids.String(ne.ID)), zap.String("node_id", ne.NodeID))
	d.persist(ctx, ctx, ne, compiled, permanentOutcome(code, err, nil))
}

func permanentOutcome(code string, err error, metrics []byte) outcome {
	return outcome{status: "failed", code: code, err: errorJSON(code, err, false), metrics: metrics}
}

// execute turns an executor panic into a permanent failure of this node rather
// than a worker crash. A panic is a bug; retrying it repeats the stack (03 §9.2).
func (d *Dispatcher) execute(ctx context.Context, ex executor.Executor, ne gen.NodeExecution, req executor.Request) (res executor.Result, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			d.logger.Error("executor panic", zap.Any("panic", recovered), zap.ByteString("stack", debug.Stack()),
				zap.String("node_execution_id", ids.String(ne.ID)), zap.String("node_type", ne.NodeType))
			res = executor.Result{}
			err = &executor.PermanentError{Code: "executor_panic", Err: errors.New("executor panicked")}
		}
	}()
	return ex.Execute(ctx, req)
}

func (d *Dispatcher) leaseOwner() pgtype.Text {
	return pgtype.Text{String: d.cfg.ID, Valid: true}
}

// attemptContext bounds one attempt by the node timeout. Without one, the
// attempt is bounded by the lease heartbeat and the run deadline only.
func attemptContext(ctx context.Context, node spec.NodeSpec) (context.Context, context.CancelFunc) {
	if node.TimeoutNS == nil {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(*node.TimeoutNS))
}

// encodeMetrics adds the engine-measured duration to executor metrics. Values the
// executor supplied must not block the result write, so unencodable metrics are
// replaced by the duration alone.
func encodeMetrics(metrics map[string]any, duration time.Duration) []byte {
	out := make(map[string]any, len(metrics)+1)
	maps.Copy(out, metrics)
	out["duration_ms"] = duration.Milliseconds()
	encoded, err := json.Marshal(out)
	if err != nil {
		encoded, _ = json.Marshal(map[string]any{"duration_ms": duration.Milliseconds()})
	}
	return encoded
}

// waitSchema stores a wait_event payload schema; JSON null means none.
func waitSchema(schema json.RawMessage) []byte {
	if len(schema) == 0 || string(schema) == "null" {
		return nil
	}
	return schema
}
