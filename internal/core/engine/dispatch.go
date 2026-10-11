package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/core/expr"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/spec"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/obs"
	"github.com/kartikey/workflow_service/pkg/uid"
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
	cfg        Config
	runs       *runs.Service
	store      *gen.Queries
	registry   *executor.Registry
	secrets    SecretSource
	logger     *zap.Logger
	maxPayload int
}

// NewDispatcher shares flow's spec cache and expression environment across all
// node executions this worker runs. secrets may be nil.
func NewDispatcher(cfg Config, f *runs.Service, registry *executor.Registry, secrets SecretSource, logger *zap.Logger) *Dispatcher {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Dispatcher{
		cfg: cfg, runs: f, store: f.Q, registry: registry, secrets: secrets,
		logger: logger, maxPayload: DefaultMaxPayloadBytes,
	}
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
		zap.String("run_id", uid.String(ne.RunID)), zap.String("node_id", ne.NodeID),
		zap.Int32("attempt", ne.Attempt), zap.String("node_execution_id", uid.String(ne.ID)),
		zap.String("worker_id", d.cfg.ID),
	)

	// 1. Load the run and its compiled spec. Database errors are transient:
	// returning lets the lease expire and the node be re-delivered (same attempt).
	run, err := d.store.GetRun(ctx, gen.GetRunParams{ID: ne.RunID, TenantID: ne.TenantID})
	if err != nil {
		log.Error("load run for node execution", zap.Error(err))
		return
	}
	if runs.IsTerminalRun(run.Status) {
		d.cancelStale(parent, ne, nil)
		return
	}
	compiled, err := d.runs.Specs.Get(ctx, d.store, run.WorkflowVersionID, run.TenantID)
	if errors.Is(err, runs.ErrSpecInvalid) {
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
	bindings, err := runs.Bindings(ctx, d.store, run, ne.BranchKey)
	if err != nil {
		log.Error("build expression bindings", zap.Error(err))
		return
	}

	// 2b. foreach: the controller row fans out and parks; each branch row runs
	// the node once with item and index bound to its element.
	if node.Foreach != nil {
		if ne.BranchKey == "" {
			d.fanOut(parent, ctx, ne, compiled, node, bindings)
			return
		}
		if bindings, err = branchBindings(ne, bindings); err != nil {
			d.failPermanent(ctx, ne, compiled, "E_FOREACH_ITEM", err)
			return
		}
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
	stored, resolvedCfg, secretValues, err := d.resolveConfig(ctx, run, node, programs.Config, bindings)
	var missing *missingSecretError
	if errors.As(err, &missing) {
		d.failPermanent(ctx, ne, compiled, "E_SECRET_MISSING", err)
		return
	}
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

	// 5. Execute outside any transaction, bounded by the node timeout. Values the
	// executor fetches itself (connection credentials) join the secrets in the
	// scrubber through req.Redact.
	redactions := newRedactions(secretValues)
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
		Redact:          redactions.Add,
	}
	attemptCtx, cancelAttempt := attemptContext(ctx, node)
	defer cancelAttempt()
	if deadline, ok := attemptCtx.Deadline(); ok {
		req.Deadline = deadline
	}
	started := time.Now()
	res, execErr := d.execute(attemptCtx, ex, ne, req)
	if values := redactions.Values(); len(values) > 0 {
		// An upstream can echo a credential back, and transport errors can quote a
		// URL that carried one: neither may reach the stored output or error.
		res.Output = scrubBytes(res.Output, values)
		execErr = scrubError(execErr, values)
	}
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

func (d *Dispatcher) resolveConfig(ctx context.Context, run gen.Run, node spec.NodeSpec, template *expr.Template, bindings expr.Bindings) (stored, resolved []byte, secretValues []string, err error) {
	if template == nil {
		return []byte(`{}`), []byte(`{}`), nil, nil
	}
	usesSecrets, refs := secretUses(node.Config)
	if usesSecrets && d.secrets != nil {
		values, err := d.secrets.Secrets(ctx, run.TenantID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("load vault secrets: %w", err)
		}
		if missing := missingSecrets(refs, values); len(missing) > 0 {
			return nil, nil, nil, &missingSecretError{names: missing}
		}
		real := bindings
		real.Secrets = values
		value, err := template.Resolve(ctx, real)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resolve config: %w", err)
		}
		if resolved, err = json.Marshal(value); err != nil {
			return nil, nil, nil, fmt.Errorf("encode resolved config: %w", err)
		}
		masked := bindings
		masked.Secrets = maskSecrets(values)
		value, err = template.Resolve(ctx, masked)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resolve config: %w", err)
		}
		stored, err = json.Marshal(value)
		return stored, resolved, secretValueList(values), err
	}
	if usesSecrets {
		// No vault is configured: say so, instead of a generic expression failure.
		if missing := missingSecrets(refs, nil); len(missing) > 0 {
			return nil, nil, nil, &missingSecretError{names: missing}
		}
	}
	value, err := template.Resolve(ctx, bindings)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve config: %w", err)
	}
	resolved, err = json.Marshal(value)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode resolved config: %w", err)
	}
	return resolved, resolved, nil, nil
}

// execute turns an executor panic into a permanent failure of this node rather
// than a worker crash. A panic is a bug; retrying it repeats the stack (03 §9.2).
func (d *Dispatcher) execute(ctx context.Context, ex executor.Executor, ne gen.NodeExecution, req executor.Request) (res executor.Result, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			d.logger.Error("executor panic", zap.Any("panic", recovered), zap.ByteString("stack", debug.Stack()),
				zap.String("node_execution_id", uid.String(ne.ID)), zap.String("node_type", ne.NodeType))
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
