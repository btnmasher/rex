// Package jobs implements a generic job runner that executes registered steps
// for claimed work items from a pluggable job store.
//
// High-level flow:
//  1. Start or resume a run in the store.
//  2. Produce work items into the run when the job has a producer.
//  3. Claim ready items in batches.
//  4. Execute all registered steps for each claimed item.
//  5. Persist terminal state: done, retry, or final failure.
//
// Domain packages own their concrete work item type and embed ItemMeta to
// satisfy runtime metadata requirements.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/btnmasher/rex/jobruntime/jobstore"
	"golang.org/x/sync/errgroup"
)

const (
	defaultProduceBatchSize  = 1000
	defaultWorkBatchSize     = 100
	defaultWorkerConcurrency = 20
	defaultMaxAttempts       = 6
	defaultBackoffSeconds    = 2
	defaultBackoffMinutes    = 2
	pendingPollDelay         = time.Second
	failureMarkTimeout       = 5 * time.Second
)

// Params identifies a run invocation and its trigger metadata.
type Params struct {
	JobID   string
	JobKind string
	// Trigger carries run origin metadata (scheduled/manual/retry).
	Trigger Trigger
	// RetryGroupID is set when processing a retry-group run.
	RetryGroupID *string
}

type runnerConfig struct {
	ProduceBatchSize  int
	WorkBatchSize     int
	WorkerConcurrency int
	MaxAttempts       int
	BackoffBase       time.Duration
	BackoffMax        time.Duration
}

func defaultRunnerConfig() runnerConfig {
	return runnerConfig{
		ProduceBatchSize:  defaultProduceBatchSize,
		WorkBatchSize:     defaultWorkBatchSize,
		WorkerConcurrency: defaultWorkerConcurrency,
		MaxAttempts:       defaultMaxAttempts,
		BackoffBase:       defaultBackoffSeconds * time.Second,
		BackoffMax:        defaultBackoffMinutes * time.Minute,
	}
}

// Runner executes typed work items through a registered workflow.
type Runner[W RuntimeItem] struct {
	store            jobstore.Store
	codec            WorkItemCodec[W]
	steps            []StepDefinition[W]
	config           runnerConfig
	logger           *slog.Logger
	progressReporter ProgressReporter
	producer         Producer
}

type runnerOptions struct {
	config   runnerConfig
	logger   *slog.Logger
	producer Producer
}

// RunnerOption configures a Runner during construction.
type RunnerOption func(*runnerOptions) error

// WithProduceBatchSize sets the maximum work items requested per producer call.
func WithProduceBatchSize(size int) RunnerOption {
	return func(opts *runnerOptions) error {
		if size > 0 {
			opts.config.ProduceBatchSize = size
		}
		return nil
	}
}

// WithWorkBatchSize sets the maximum number of claimed items per iteration.
func WithWorkBatchSize(size int) RunnerOption {
	return func(opts *runnerOptions) error {
		if size > 0 {
			opts.config.WorkBatchSize = size
		}
		return nil
	}
}

// WithWorkerConcurrency sets the maximum number of item workflows in flight.
func WithWorkerConcurrency(workers int) RunnerOption {
	return func(opts *runnerOptions) error {
		if workers > 0 {
			opts.config.WorkerConcurrency = workers
		}
		return nil
	}
}

// WithMaxAttempts sets the terminal retry threshold for one item.
func WithMaxAttempts(maxAttempts int) RunnerOption {
	return func(opts *runnerOptions) error {
		if maxAttempts > 0 {
			opts.config.MaxAttempts = maxAttempts
		}
		return nil
	}
}

// WithBackoff sets the base and maximum retry delays.
func WithBackoff(base, maxDelay time.Duration) RunnerOption {
	return func(opts *runnerOptions) error {
		if base > 0 {
			opts.config.BackoffBase = base
		}
		if maxDelay > 0 {
			opts.config.BackoffMax = maxDelay
		}
		return nil
	}
}

// WithLogger supplies the structured logger used by the runner.
func WithLogger(logger *slog.Logger) RunnerOption {
	return func(opts *runnerOptions) error {
		if logger == nil {
			return nil
		}
		opts.logger = logger
		return nil
	}
}

// WithProducer supplies an optional domain-owned work producer.
func WithProducer(producer Producer) RunnerOption {
	return func(opts *runnerOptions) error {
		if producer == nil {
			return fmt.Errorf("job producer must not be nil")
		}
		opts.producer = producer
		return nil
	}
}

func NewRunner[W RuntimeItem](
	store jobstore.Store,
	codec WorkItemCodec[W],
	opts ...RunnerOption,
) (*Runner[W], error) {
	if store == nil {
		return nil, fmt.Errorf("job store is required")
	}
	if codec == nil {
		return nil, fmt.Errorf("work item codec is required")
	}

	runnerOpts := runnerOptions{
		config: defaultRunnerConfig(),
		logger: slog.Default(),
	}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(&runnerOpts); err != nil {
			return nil, err
		}
	}

	runner := &Runner[W]{
		store:    store,
		codec:    codec,
		config:   runnerOpts.config,
		logger:   runnerOpts.logger,
		producer: runnerOpts.producer,
	}

	return runner, nil
}

// SetProgressReporter installs an optional in-flight progress sink.
func (r *Runner[W]) SetProgressReporter(reporter ProgressReporter) {
	r.progressReporter = reporter
}

// RegisterStep appends one uniquely named workflow step.
func (r *Runner[W]) RegisterStep(step StepDefinition[W]) error {
	if step.Name == "" {
		return fmt.Errorf("step name is required")
	}

	if step.Delegate == nil {
		return fmt.Errorf("step %s delegate is required", step.Name)
	}

	for _, existing := range r.steps {
		if existing.Name == step.Name {
			return fmt.Errorf("duplicate step name %s", step.Name)
		}
	}

	r.steps = append(r.steps, step)

	return nil
}

// RegisterSteps appends workflow steps in execution order.
func (r *Runner[W]) RegisterSteps(steps []StepDefinition[W]) error {
	for _, step := range steps {
		if err := r.RegisterStep(step); err != nil {
			return err
		}
	}

	return nil
}

// StartOrResumeRun acquires a run slot and processes it to completion.
func (r *Runner[W]) StartOrResumeRun(ctx context.Context, p *Params) (runID string, err error) {
	defer func() {
		if panicValue := recover(); panicValue != nil {
			err = r.handleRunPanic(ctx, runID, panicValue, debug.Stack())
		}
	}()

	if p == nil || p.JobID == "" || p.JobKind == "" {
		return "", fmt.Errorf("job parameters are incomplete")
	}
	start, err := r.store.StartOrGetRun(ctx, &jobstore.StartRunRequest{
		JobID:         p.JobID,
		JobKind:       p.JobKind,
		TriggerType:   string(p.Trigger.Type),
		TriggerEntity: p.Trigger.Entity,
		TriggerMeta:   p.Trigger.Meta,
		RetryGroupID:  p.RetryGroupID,
	})

	if err != nil {
		return "", err
	}
	if start.ExistingRunning {
		return start.RunID, nil
	}

	meta := JobMeta{
		JobID:   p.JobID,
		JobKind: p.JobKind,
		RunID:   start.RunID,
		SlotTS:  start.SlotTS,
		Trigger: p.Trigger,
	}

	if err := r.RunToCompletion(ctx, &meta, p.RetryGroupID); err != nil {
		failureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureMarkTimeout)
		defer cancel()
		if markErr := r.store.MarkRunFailed(failureCtx, start.RunID, err.Error()); markErr != nil {
			r.logger.Warn("mark run failed", "run_id", start.RunID, "err", markErr)
		}
		return start.RunID, err
	}

	return start.RunID, nil
}

// RunRetryGroup executes items associated with an existing retry group.
func (r *Runner[W]) RunRetryGroup(ctx context.Context, retryGroupID string) (runID string, err error) {
	defer func() {
		if panicValue := recover(); panicValue != nil {
			panicErr := r.handleRunPanic(ctx, runID, panicValue, debug.Stack())
			if runID != "" {
				r.recoverRetryGroup(ctx, retryGroupID, runID, panicErr)
			}
			err = panicErr
		}
	}()

	run, err := r.store.StartRetryGroup(ctx, retryGroupID)
	if err != nil {
		return "", err
	}
	runID = run.RunID

	meta := JobMeta{
		JobID:   run.JobID,
		JobKind: run.JobKind,
		RunID:   run.RunID,
		SlotTS:  time.Now().UTC(),
		Trigger: Trigger{Type: TriggerRetry, Entity: "retry_group", Meta: map[string]any{"retry_group_id": retryGroupID}},
	}

	if err := r.RunToCompletion(ctx, &meta, &retryGroupID); err != nil {
		r.recoverRetryGroup(ctx, retryGroupID, run.RunID, err)
		return run.RunID, err
	}

	if err := r.store.CompleteRetryGroup(ctx, retryGroupID); err != nil {
		r.recoverRetryGroup(ctx, retryGroupID, run.RunID, err)
		return run.RunID, err
	}

	return run.RunID, nil
}

func (r *Runner[W]) recoverRetryGroup(ctx context.Context, retryGroupID, runID string, runErr error) {
	recoveryContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureMarkTimeout)
	defer cancel()
	if err := r.store.MarkRunFailed(recoveryContext, runID, runErr.Error()); err != nil {
		r.logger.Warn("mark retry run failed", "run_id", runID, "err", err)
	}
	if err := r.store.ReopenRetryGroup(recoveryContext, retryGroupID); err != nil {
		r.logger.Warn("reopen retry group", "retry_group_id", retryGroupID, "err", err)
	}
}

func (r *Runner[W]) handleRunPanic(ctx context.Context, runID string, panicValue any, stack []byte) error {
	panicErr := newPanicError("run lifecycle", panicValue)
	logger := r.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("job runtime panic recovered",
		"run_id", runID,
		"panic_type", fmt.Sprintf("%T", panicValue),
		"stack", string(stack),
	)
	if runID != "" {
		recoveryContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureMarkTimeout)
		r.markRunFailedAfterPanic(recoveryContext, runID, panicErr)
		cancel()
	}
	return panicErr
}

func (r *Runner[W]) markRunFailedAfterPanic(ctx context.Context, runID string, runErr error) {
	logger := r.logger
	if logger == nil {
		logger = slog.Default()
	}
	panicked, panicValue, stack := capturePanic(func() {
		if markErr := r.store.MarkRunFailed(ctx, runID, runErr.Error()); markErr != nil {
			logger.Warn("mark panicked run failed", "run_id", runID, "err", markErr)
		}
	})
	if panicked {
		logger.Error("mark panicked run failed with panic",
			"run_id", runID,
			"panic_type", fmt.Sprintf("%T", panicValue),
			"stack", string(stack),
		)
	}
}

// RunOnce performs a single stage+claim+process iteration for a run.
func (r *Runner[W]) RunOnce(ctx context.Context, runID string) (err error) {
	defer func() {
		if panicValue := recover(); panicValue != nil {
			err = r.handleRunPanic(ctx, runID, panicValue, debug.Stack())
		}
	}()

	if err := ctx.Err(); err != nil {
		return err
	}

	meta := &JobMeta{
		RunID: runID,
	}

	if _, err := r.produceItemsIfNeeded(ctx, meta, nil); err != nil {
		return err
	}

	claim, err := r.store.ClaimItems(ctx, jobstore.ClaimRequest{
		RunID:        runID,
		RetryGroupID: nil,
		BatchSize:    r.config.WorkBatchSize,
	})

	if err != nil {
		return &PipelineError{
			Stage: "ITEM_CLAIM",
			Err:   err,
		}
	}

	if len(claim.Items) == 0 {
		return nil
	}

	hardFailures, err := r.processClaimedItems(ctx, meta, nil, claim.Items)
	if err != nil {
		return err
	}

	if hardFailures > 0 {
		return &PipelineError{
			Stage: "ITEM_WORKFLOW",
			Err:   fmt.Errorf("%d item(s) failed in critical step", hardFailures),
		}
	}
	return nil
}

// RunToCompletion repeatedly processes iterations until no more items remain.
func (r *Runner[W]) RunToCompletion(ctx context.Context, meta *JobMeta, retryGroupID *string) (err error) {
	runID := ""
	if meta != nil {
		runID = meta.RunID
	}
	defer func() {
		if panicValue := recover(); panicValue != nil {
			err = r.handleRunPanic(ctx, runID, panicValue, debug.Stack())
		}
	}()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		keepRunning, err := r.runToCompletionIteration(ctx, meta, retryGroupID)
		if err != nil {
			return err
		}
		if !keepRunning {
			return nil
		}
	}
}

func (r *Runner[W]) runToCompletionIteration(ctx context.Context, meta *JobMeta, retryGroupID *string) (bool, error) {
	if err := r.store.Heartbeat(ctx, meta.RunID); err != nil {
		r.logger.Warn("heartbeat failed", "err", err)
	}

	productionDone, err := r.produceItemsIfNeeded(ctx, meta, retryGroupID)
	if err != nil {
		return false, err
	}

	claim, err := r.store.ClaimItems(ctx, jobstore.ClaimRequest{
		RunID:        meta.RunID,
		RetryGroupID: retryGroupID,
		BatchSize:    r.config.WorkBatchSize,
	})

	if err != nil {
		return false, &PipelineError{Stage: "ITEM_CLAIM", Err: err}
	}

	if len(claim.Items) == 0 {
		return r.handleEmptyClaim(ctx, meta.RunID, productionDone, claim)
	}

	hardFailures, err := r.processClaimedItems(ctx, meta, retryGroupID, claim.Items)
	if err != nil {
		return false, err
	}

	if hardFailures > 0 {
		return false, &PipelineError{Stage: "ITEM_WORKFLOW", Err: fmt.Errorf("%d item(s) failed in critical step", hardFailures)}
	}
	return true, nil
}

func (r *Runner[W]) handleEmptyClaim(ctx context.Context, runID string, productionDone bool, claim jobstore.ClaimResult) (bool, error) {
	if claim.Pending {
		if err := waitForPending(ctx, claim.NextRetryAt); err != nil {
			return false, err
		}
		return true, nil
	}
	if !productionDone {
		return true, nil
	}
	return false, r.completeRun(ctx, runID)
}

func (r *Runner[W]) produceItemsIfNeeded(ctx context.Context, meta *JobMeta, retryGroupID *string) (bool, error) {
	if retryGroupID != nil {
		return true, nil
	}
	if r.producer == nil {
		return true, nil
	}

	checkpoint, err := r.store.GetRunCheckpoint(ctx, meta.RunID)
	if err != nil {
		return false, err
	}

	produced, err := r.producer.Produce(ctx, ProduceRequest{
		RunID:      meta.RunID,
		Checkpoint: checkpoint,
		BatchSize:  r.config.ProduceBatchSize,
	})

	if err != nil {
		return false, &PipelineError{Stage: "PRODUCE", Err: err}
	}
	if err := produced.NextCheckpoint.Validate(); err != nil {
		return false, &PipelineError{Stage: "CHECKPOINT", Err: err}
	}
	if err := r.store.InsertItems(ctx, meta.RunID, produced.Items); err != nil {
		return false, &PipelineError{Stage: "QUEUE", Err: err}
	}
	if !produced.NextCheckpoint.Empty() {
		if err := r.store.SaveRunCheckpoint(ctx, meta.RunID, produced.NextCheckpoint); err != nil {
			return false, &PipelineError{Stage: "CHECKPOINT", Err: err}
		}
	}

	if len(produced.Items) > 0 {
		r.logger.Info("produced_items", "run_id", meta.RunID, "count", len(produced.Items))
	}

	return produced.Done, nil
}

func (r *Runner[W]) completeRun(ctx context.Context, runID string) error {
	stats, err := r.store.CountItemsByState(ctx, runID)
	if err != nil {
		return fmt.Errorf("count run items: %w", err)
	}
	return r.store.MarkRunCompleted(ctx, runID, map[string]any{"counts": stats})
}

func waitForPending(ctx context.Context, nextRetryAt *time.Time) error {
	delay := pendingPollDelay
	if nextRetryAt != nil {
		delay = max(time.Until(*nextRetryAt), time.Duration(0))
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *Runner[W]) processClaimedItems(ctx context.Context, meta *JobMeta, retryGroupID *string, items []jobstore.WorkItem) (int, error) {
	var group errgroup.Group
	group.SetLimit(r.config.WorkerConcurrency)
	var hardFailures int32
	for i := range items {
		item := items[i]
		group.Go(func() error {
			mustFail, err := r.processClaimedItemSafely(ctx, meta, retryGroupID, &item)
			if err != nil {
				return err
			}
			if mustFail {
				atomic.AddInt32(&hardFailures, 1)
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return 0, err
	}
	return int(hardFailures), nil
}

func (r *Runner[W]) processClaimedItemSafely(ctx context.Context, meta *JobMeta, retryGroupID *string, item *jobstore.WorkItem) (mustFail bool, err error) {
	panicked, panicValue, stack := capturePanic(func() {
		mustFail, err = r.processClaimedItem(ctx, meta, retryGroupID, item)
	})
	if !panicked {
		return mustFail, err
	}

	panicErr := newPanicError("item workflow", panicValue)
	r.logger.Error("job item panic recovered",
		"run_id", meta.RunID,
		"item_id", item.ItemID,
		"panic_type", fmt.Sprintf("%T", panicValue),
		"stack", string(stack),
	)

	var retryMustFail bool
	var retryErr error
	retryPanicked, retryPanicValue, retryStack := capturePanic(func() {
		retryMustFail, retryErr = r.retryClaimedPanic(ctx, meta.RunID, item, panicErr)
	})
	if retryPanicked {
		r.logger.Error("job item panic recovery failed",
			"run_id", meta.RunID,
			"item_id", item.ItemID,
			"panic_type", fmt.Sprintf("%T", retryPanicValue),
			"stack", string(retryStack),
		)
		return false, errors.Join(panicErr, newPanicError("item panic recovery", retryPanicValue))
	}

	return retryMustFail, retryErr
}

func (r *Runner[W]) retryClaimedPanic(ctx context.Context, runID string, item *jobstore.WorkItem, panicErr error) (bool, error) {
	mutationCtx, cancel := claimMutationContext(ctx)
	defer cancel()

	if item.AttemptCount >= r.config.MaxAttempts {
		if err := r.store.MarkItemFailedFinal(mutationCtx, jobstore.FailureRecord{
			RunID:        runID,
			ItemID:       item.ItemID,
			AttemptCount: item.AttemptCount,
			ErrorMsg:     "max attempts reached: " + panicErr.Error(),
		}); err != nil {
			return false, &PipelineError{Stage: "ITEM_MARK_FAIL", Err: err}
		}

		return true, nil
	}

	tryAt := time.Now().UTC().Add(NextBackoff(item.AttemptCount, r.config.BackoffBase, r.config.BackoffMax))
	if err := r.store.MarkItemRetry(mutationCtx, &jobstore.RetryRecord{
		RunID:        runID,
		ItemID:       item.ItemID,
		AttemptCount: item.AttemptCount,
		Attempt:      item.AttemptCount,
		RetryAt:      tryAt,
		ErrorMsg:     panicErr.Error(),
	}); err != nil {
		return false, &PipelineError{Stage: "ITEM_MARK_RETRY", Err: err}
	}

	return false, nil
}

func (r *Runner[W]) processClaimedItem(ctx context.Context, meta *JobMeta, retryGroupID *string, item *jobstore.WorkItem) (bool, error) {
	decoded, err := r.decodeClaimedItem(item, retryGroupID)
	if err != nil {
		if markErr := r.markClaimedDecodeFailure(ctx, meta.RunID, item, err); markErr != nil {
			return false, errors.Join(err, markErr)
		}
		return true, nil
	}

	mustFail, err := r.executeItemWorkflow(ctx, meta, retryGroupID, decoded)
	if err == nil {
		return mustFail, nil
	}
	mustFail, markErr := r.retryClaimedItem(ctx, meta.RunID, decoded, err)
	if markErr != nil {
		return false, errors.Join(err, markErr)
	}
	return mustFail, nil
}

func (r *Runner[W]) markClaimedDecodeFailure(ctx context.Context, runID string, item *jobstore.WorkItem, decodeErr error) error {
	mutationCtx, cancel := claimMutationContext(ctx)
	defer cancel()
	return r.store.MarkItemFailedFinal(mutationCtx, jobstore.FailureRecord{
		RunID:        runID,
		ItemID:       item.ItemID,
		AttemptCount: item.AttemptCount,
		ErrorMsg:     "decode item: " + decodeErr.Error(),
	})
}

func (r *Runner[W]) retryClaimedItem(ctx context.Context, runID string, item W, workflowErr error) (bool, error) {
	mutationCtx, cancel := claimMutationContext(ctx)
	defer cancel()
	return r.markRetry(mutationCtx, runID, item, &StepResult[W]{
		Item:   item,
		Status: ItemStatusRetry,
		Error:  workflowErr.Error(),
	}, true)
}

func claimMutationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), failureMarkTimeout)
}

func (r *Runner[W]) decodeClaimedItem(item *jobstore.WorkItem, retryGroupID *string) (W, error) {
	return r.codec.DecodeItem(
		ItemMeta{
			ItemID:       item.ItemID,
			AttemptCount: item.AttemptCount,
			RetryGroupID: retryGroupID,
		},
		item.PayloadJSON,
		item.ResultJSON,
	)
}

func (r *Runner[W]) executeItemWorkflow(ctx context.Context, meta *JobMeta, retryGroupID *string, current W) (bool, error) {
	for _, def := range r.steps {
		rt := newStepRuntime(&stepRuntimeParams{
			store:            r.store,
			logger:           r.logger,
			runID:            meta.RunID,
			jobID:            meta.JobID,
			jobKind:          meta.JobKind,
			step:             def.Name,
			retryGroupID:     retryGroupID,
			progressReporter: r.progressReporter,
		})
		results, err := def.Delegate(ctx, rt, []W{current})
		if err != nil {
			return false, &PipelineError{Stage: def.Name + "_DELEGATE", Err: err}
		}

		res := findStepResult(current.ID(), results)
		requireHardFail := def.Critical || !def.AllowPartialFailure
		done, mustFailRun, err := r.applyStepResult(ctx, meta, &def, current, &res, requireHardFail)
		if err != nil {
			return false, err
		}
		if done {
			return mustFailRun, nil
		}
		current = res.Item
	}

	resultJSON, err := r.codec.EncodeResult(current)
	if err != nil {
		return false, err
	}

	if err := r.store.MarkItemCompleted(ctx, &jobstore.SuccessRecord{
		RunID:        meta.RunID,
		ItemID:       current.ID(),
		AttemptCount: current.Attempts(),
		ResultJSON:   resultJSON,
	}); err != nil {
		return false, &PipelineError{Stage: "ITEM_MARK_DONE", Err: err}
	}

	return false, nil
}

func (r *Runner[W]) applyStepResult(
	ctx context.Context,
	meta *JobMeta,
	def *StepDefinition[W],
	current W,
	res *StepResult[W],
	requireHardFail bool,
) (done, mustFailRun bool, err error) {
	switch res.Status {
	case ItemStatusSucceeded:
		return false, false, nil
	case ItemStatusRetry:
		mustFail, retryErr := r.markRetry(ctx, meta.RunID, current, res, requireHardFail)
		return true, mustFail, retryErr
	case ItemStatusCanceled:
		err = r.markItemFailed(ctx, meta.RunID, current, "canceled", def.Name)
		return true, requireHardFail, err
	case ItemStatusFailed:
		err = r.markItemFailed(ctx, meta.RunID, current, res.Error, def.Name)
		return true, requireHardFail, err
	default:
		err = r.markItemFailed(ctx, meta.RunID, current, "unknown item status", def.Name)
		return true, requireHardFail, err
	}
}

func (r *Runner[W]) markItemFailed(ctx context.Context, runID string, current W, errMsg, stepName string) error {
	if err := r.store.MarkItemFailedFinal(ctx, jobstore.FailureRecord{
		RunID:        runID,
		ItemID:       current.ID(),
		AttemptCount: current.Attempts(),
		ErrorMsg:     errMsg,
	}); err != nil {
		return &PipelineError{
			Stage: stepName + "_MARK_FAIL",
			Err:   err,
		}
	}
	return nil
}

func (r *Runner[W]) markRetry(ctx context.Context, runID string, current W, res *StepResult[W], requireHardFail bool) (bool, error) {
	attempt := current.Attempts()
	if attempt >= r.config.MaxAttempts {
		if err := r.store.MarkItemFailedFinal(ctx, jobstore.FailureRecord{
			RunID:        runID,
			ItemID:       current.ID(),
			AttemptCount: current.Attempts(),
			ErrorMsg:     "max attempts reached: " + res.Error,
		}); err != nil {
			return false, &PipelineError{Stage: "ITEM_MARK_FAIL", Err: err}
		}

		return requireHardFail, nil
	}
	tryAt := time.Now().UTC().Add(NextBackoff(attempt, r.config.BackoffBase, r.config.BackoffMax))
	if res.RetryOverride && res.RetryAt != nil {
		tryAt = res.RetryAt.UTC()
	}

	if err := r.store.MarkItemRetry(ctx, &jobstore.RetryRecord{
		RunID:        runID,
		ItemID:       current.ID(),
		AttemptCount: current.Attempts(),
		Attempt:      attempt,
		RetryAt:      tryAt,
		ErrorMsg:     res.Error,
	}); err != nil {
		return false, &PipelineError{Stage: "ITEM_MARK_RETRY", Err: err}
	}

	return false, nil
}

func findStepResult[W RuntimeItem](itemID ItemID, results []StepResult[W]) StepResult[W] {
	for _, res := range results {
		if res.Item.ID() == itemID {
			return res
		}
	}

	return StepResult[W]{Status: ItemStatusFailed, Error: "delegate did not return result for claimed item"}
}
