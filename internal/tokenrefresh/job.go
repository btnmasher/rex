// Package tokenrefresh defines the scheduled auth-next access-token refresh job.
package tokenrefresh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/btnmasher/rex/jobruntime/jobs"
	"github.com/btnmasher/rex/jobruntime/jobstore"
)

const (
	jobID              = "rex"
	jobKind            = "access-token-refresh"
	refreshInterval    = 15 * time.Minute
	defaultConcurrency = 8
	maxAttempts        = 6
)

// CorporationSource lists eligible corporations.
type CorporationSource interface {
	ListCorporations(context.Context) ([]string, error)
}

// TokenStore receives refreshed credentials and owns token rotation state.
type TokenStore interface {
	SetEligibleCorporations([]string)
	RefreshCorporationIfDue(context.Context, string, time.Duration) (bool, error)
	TokenCount(string) int
	HasUsableTokens() bool
}

// CorporationWorkItem is the safe, storage-backed unit of one corporation refresh.
type CorporationWorkItem struct {
	jobs.ItemMeta

	CorporationID string `json:"corporationId"`
}

type refreshCheckpoint struct {
	Listed bool `json:"listed"`
}

type corporationCodec struct{}

func (corporationCodec) DecodeItem(meta jobs.ItemMeta, payloadJSON, _ []byte) (CorporationWorkItem, error) {
	var payload struct {
		CorporationID string `json:"corporationId"`
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return CorporationWorkItem{}, fmt.Errorf("decode corporation refresh item: %w", err)
	}
	if payload.CorporationID == "" {
		return CorporationWorkItem{}, errors.New("corporation refresh item has no corporation ID")
	}
	return CorporationWorkItem{ItemMeta: meta, CorporationID: payload.CorporationID}, nil
}

func (corporationCodec) EncodeResult(CorporationWorkItem) ([]byte, error) {
	return nil, nil
}

type corporationProducer struct {
	source CorporationSource
	store  TokenStore
}

func (p corporationProducer) Produce(
	ctx context.Context,
	request jobs.TypedProduceRequest[refreshCheckpoint],
) (jobs.TypedProduceResult[refreshCheckpoint], error) {
	if request.Checkpoint.Listed {
		return jobs.TypedProduceResult[refreshCheckpoint]{Done: true, HasCheckpoint: true, NextCheckpoint: request.Checkpoint}, nil
	}
	corporationIDs, err := p.source.ListCorporations(ctx)
	if err != nil {
		return jobs.TypedProduceResult[refreshCheckpoint]{}, fmt.Errorf("list eligible corporations: %w", err)
	}
	corporationIDs = append([]string(nil), corporationIDs...)
	sort.Strings(corporationIDs)
	p.store.SetEligibleCorporations(corporationIDs)
	items := make([]jobstore.WorkItem, 0, len(corporationIDs))
	for _, corporationID := range corporationIDs {
		payload, marshalErr := json.Marshal(struct {
			CorporationID string `json:"corporationId"`
		}{CorporationID: corporationID})
		if marshalErr != nil {
			return jobs.TypedProduceResult[refreshCheckpoint]{}, fmt.Errorf("encode corporation %s: %w", corporationID, marshalErr)
		}
		items = append(items, jobstore.WorkItem{ItemID: jobstore.ItemID(corporationID), PayloadJSON: payload})
	}
	return jobs.TypedProduceResult[refreshCheckpoint]{
		Items:          items,
		Done:           true,
		HasCheckpoint:  true,
		NextCheckpoint: refreshCheckpoint{Listed: true},
	}, nil
}

// Job is the scheduled access-token refresh workflow.
type Job struct {
	runner    *jobs.Runner[CorporationWorkItem]
	store     TokenStore
	logger    *slog.Logger
	ready     chan struct{}
	readyOnce sync.Once
}

// New creates a refresh job with bounded corporation fanout.
func New(store jobstore.Store, source CorporationSource, tokens TokenStore, concurrency int, logger *slog.Logger) (*Job, error) {
	if store == nil || source == nil || tokens == nil {
		return nil, errors.New("token refresh job dependencies are required")
	}
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	if logger == nil {
		logger = slog.Default()
	}
	codec, err := jobs.NewJSONCheckpointCodec[refreshCheckpoint](jobKind)
	if err != nil {
		return nil, err
	}
	producer, err := jobs.NewTypedProducer(codec, corporationProducer{source: source, store: tokens}.Produce)
	if err != nil {
		return nil, err
	}
	runner, err := jobs.NewRunner(
		store,
		corporationCodec{},
		jobs.WithProducer(producer),
		jobs.WithProduceBatchSize(concurrency),
		jobs.WithWorkBatchSize(concurrency),
		jobs.WithWorkerConcurrency(concurrency),
		jobs.WithMaxAttempts(maxAttempts),
		jobs.WithLogger(logger),
	)
	if err != nil {
		return nil, err
	}
	job := &Job{
		runner: runner,
		store:  tokens,
		logger: logger,
		ready:  make(chan struct{}),
	}
	if err := runner.RegisterStep(jobs.StepDefinition[CorporationWorkItem]{
		Name:                "FETCH_CORPORATION_ACCESS_TOKENS",
		AllowPartialFailure: true,
		Delegate:            job.fetchCorporation,
	}); err != nil {
		return nil, err
	}
	return job, nil
}

// Ready returns a channel closed after the first successful refresh run.
func (j *Job) Ready() <-chan struct{} {
	return j.ready
}

// Run executes one refresh run. The store prevents overlapping runs.
func (j *Job) Run(ctx context.Context) (string, error) {
	runID, err := j.runner.StartOrResumeRun(ctx, &jobs.Params{
		JobID:   jobID,
		JobKind: jobKind,
		Trigger: jobs.Trigger{Type: jobs.TriggerScheduled, Entity: "interval"},
	})
	if err == nil && j.store.HasUsableTokens() {
		j.readyOnce.Do(func() { close(j.ready) })
	}
	return runID, err
}

// RunPeriodically starts another refresh every fifteen minutes until cancellation.
func (j *Job) RunPeriodically(ctx context.Context) error {
	j.logger.Info("access-token refresh scheduler started", "interval", refreshInterval)
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			j.logger.Info("scheduled access-token refresh starting")
			j.runAndLog(ctx)
		}
	}
}

func (j *Job) runAndLog(ctx context.Context) {
	startedAt := time.Now()
	runID, err := j.Run(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			j.logger.Error("access-token refresh job failed", "run_id", runID, "duration", time.Since(startedAt), "err", err)
		}
		return
	}
	j.logger.Info("access-token refresh job finished", "run_id", runID, "duration", time.Since(startedAt), "usable_tokens", j.store.HasUsableTokens())
}

func (j *Job) fetchCorporation(
	ctx context.Context,
	_ jobs.StepRuntime,
	items []CorporationWorkItem,
) ([]jobs.StepResult[CorporationWorkItem], error) {
	results := jobs.NewStepResultList(items)
	for _, item := range items {
		j.logger.Debug("corporation access-token refresh started", "corporation_id", item.CorporationID)
		refreshed, err := j.store.RefreshCorporationIfDue(ctx, item.CorporationID, refreshInterval)
		if err != nil {
			results = append(results, jobs.Retry(item, err))
			continue
		}
		if !refreshed {
			j.logger.Debug("corporation access-token refresh skipped",
				"corporation_id", item.CorporationID,
				"reason", "recently_refreshed",
				"minimum_age", refreshInterval,
				"token_count", j.store.TokenCount(item.CorporationID),
			)
			results = append(results, jobs.Succeeded(item))
			continue
		}
		j.logger.Debug("corporation access-token refresh completed",
			"corporation_id", item.CorporationID,
			"minimum_age", refreshInterval,
			"token_count", j.store.TokenCount(item.CorporationID),
		)
		results = append(results, jobs.Succeeded(item))
	}
	return results, nil
}
