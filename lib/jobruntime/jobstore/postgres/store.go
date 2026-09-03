// Package postgres provides the durable job store backed by SQLC and PostgreSQL.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"time"

	"github.com/btnmasher/rex/jobruntime/jobstore"
	"github.com/btnmasher/rex/jobruntime/jobstore/postgres/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultMaxConns     = 4
	defaultMinConns     = 0
	defaultConnIdleTime = 30 * time.Second
	defaultConnLifetime = 5 * time.Minute
	defaultClaimSize    = 100
	maxSQLCLimit        = int64(1<<31 - 1)
	metadataCapacity    = 3
	maxAttemptCount     = int64(1<<31 - 1)
)

var errRetryGroupNotFound = errors.New("retry group not found")

// Store implements jobstore.Store with SQLC-generated query methods.
type Store struct {
	pool     *pgxpool.Pool
	queries  *gen.Queries
	ownsPool bool
}

var _ jobstore.Store = (*Store)(nil)

// NewStore opens a bounded PostgreSQL pool for durable job state using ctx.
func NewStore(ctx context.Context, databaseURL string) (*Store, error) {
	if ctx == nil {
		return nil, errors.New("postgres database context is required")
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = defaultMaxConns
	cfg.MinConns = defaultMinConns
	cfg.MaxConnIdleTime = defaultConnIdleTime
	cfg.MaxConnLifetime = defaultConnLifetime

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate postgres schema: %w", err)
	}
	return &Store{pool: pool, queries: gen.New(pool), ownsPool: true}, nil
}

// NewStoreWithPool composes the durable store over an existing application pool using ctx.
func NewStoreWithPool(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	if ctx == nil {
		return nil, errors.New("postgres database context is required")
	}
	if pool == nil {
		return nil, errors.New("postgres job store pool is required")
	}
	if err := Migrate(ctx, pool); err != nil {
		return nil, fmt.Errorf("migrate postgres schema: %w", err)
	}
	return &Store{pool: pool, queries: gen.New(pool)}, nil
}

// Close releases the PostgreSQL connection pool when this store owns it.
func (s *Store) Close() {
	if s.ownsPool {
		s.pool.Close()
	}
}

// StartOrGetRun creates a run or returns the currently active run of its kind.
func (s *Store) StartOrGetRun(ctx context.Context, req *jobstore.StartRunRequest) (jobstore.StartRunResult, error) {
	if req == nil || req.JobKind == "" {
		return jobstore.StartRunResult{}, errors.New("job kind is required")
	}
	if reclaimed, ok, err := s.reclaimStaleRun(ctx, req.JobKind); err != nil {
		return jobstore.StartRunResult{}, err
	} else if ok {
		return reclaimed, nil
	}
	if existing, ok, err := s.runningRun(ctx, req.JobKind); err != nil {
		return jobstore.StartRunResult{}, err
	} else if ok {
		return existing, nil
	}

	now := time.Now().UTC()
	runID := fmt.Sprintf("%s:%s:%d", req.JobID, req.JobKind, now.UnixNano())
	meta, err := json.Marshal(req.TriggerMeta)
	if err != nil {
		return jobstore.StartRunResult{}, fmt.Errorf("marshal trigger metadata: %w", err)
	}
	row, err := s.queries.InsertRun(ctx, gen.InsertRunParams{
		RunID:       runID,
		JobID:       req.JobID,
		JobKind:     req.JobKind,
		SlotTs:      timestamp(now),
		TriggerType: req.TriggerType,
		Column6:     meta,
	})
	if err != nil {
		if existing, ok, lookupErr := s.runningRun(ctx, req.JobKind); lookupErr == nil && ok {
			return existing, nil
		}
		return jobstore.StartRunResult{}, fmt.Errorf("insert run: %w", err)
	}
	_ = s.RecordEvent(ctx, &jobstore.EventRecord{RunID: row.RunID, Level: "INFO", Kind: "RUN_STARTED", JobID: req.JobID, JobKind: req.JobKind})
	return jobstore.StartRunResult{RunID: row.RunID, SlotTS: row.SlotTs.Time, ExistingRunning: false}, nil
}

// MarkRunCompleted marks a run complete and persists its statistics.
func (s *Store) MarkRunCompleted(ctx context.Context, runID string, stats map[string]any) error {
	statsJSON, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("marshal run statistics: %w", err)
	}
	rows, err := s.queries.CompleteRun(ctx, gen.CompleteRunParams{RunID: runID, Column2: statsJSON})
	return expectAffected(rows, err, "complete run")
}

// MarkRunFailed marks a run failed with a redacted diagnostic supplied by the caller.
func (s *Store) MarkRunFailed(ctx context.Context, runID, errMsg string) error {
	rows, err := s.queries.FailRun(ctx, gen.FailRunParams{RunID: runID, Error: text(errMsg)})
	return expectAffected(rows, err, "fail run")
}

// Heartbeat updates a run's activity timestamp.
func (s *Store) Heartbeat(ctx context.Context, runID string) error {
	rows, err := s.queries.HeartbeatRun(ctx, runID)
	return expectAffected(rows, err, "heartbeat run")
}

// GetRunCheckpoint returns opaque producer progress for a run.
func (s *Store) GetRunCheckpoint(ctx context.Context, runID string) (jobstore.Checkpoint, error) {
	row, err := s.queries.GetRunCheckpoint(ctx, runID)
	if err != nil {
		return jobstore.Checkpoint{}, err
	}
	return jobstore.Checkpoint{Namespace: row.CheckpointNamespace, Value: append([]byte(nil), row.CheckpointValue...)}, nil
}

// SaveRunCheckpoint replaces the producer checkpoint for a run.
func (s *Store) SaveRunCheckpoint(ctx context.Context, runID string, checkpoint jobstore.Checkpoint) error {
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	rows, err := s.queries.SaveRunCheckpoint(ctx, gen.SaveRunCheckpointParams{
		RunID:               runID,
		CheckpointNamespace: checkpoint.Namespace,
		CheckpointValue:     checkpoint.Value,
	})
	return expectAffected(rows, err, "save run checkpoint")
}

// InsertItems atomically inserts ready work items into a run.
func (s *Store) InsertItems(ctx context.Context, runID string, items []jobstore.WorkItem) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := s.queries.WithTx(tx)
	for _, item := range items {
		itemID, parseErr := parseItemID(item.ItemID)
		if parseErr != nil {
			return parseErr
		}
		rows, insertErr := queries.InsertWorkItem(ctx, gen.InsertWorkItemParams{
			RunID:   runID,
			ItemID:  itemID,
			Column3: item.PayloadJSON,
			Column4: item.ResultJSON,
		})
		if insertErr != nil {
			return insertErr
		}
		if rows == 0 {
			return fmt.Errorf("insert work item %s: existing payload differs", item.ItemID)
		}
	}
	return tx.Commit(ctx)
}

// ClaimItems atomically claims ready work using SQLC's SKIP LOCKED query.
func (s *Store) ClaimItems(ctx context.Context, req jobstore.ClaimRequest) (jobstore.ClaimResult, error) {
	limit := req.BatchSize
	if limit <= 0 {
		limit = defaultClaimSize
	}
	groupID := ""
	if req.RetryGroupID != nil {
		groupID = *req.RetryGroupID
	}
	rows, err := s.queries.ClaimItems(ctx, gen.ClaimItemsParams{
		RunID:   req.RunID,
		Column2: groupID,
		Limit:   sqlcLimit(limit),
	})
	if err != nil {
		return jobstore.ClaimResult{}, err
	}
	items := make([]jobstore.WorkItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, jobstore.WorkItem{
			RunID:        row.RunID,
			ItemID:       jobstore.ItemID(strconv.FormatInt(row.ItemID, 10)),
			AttemptCount: int(row.AttemptCount),
			PayloadJSON:  row.PayloadJson,
			ResultJSON:   row.ResultJson,
		})
	}
	pending, err := s.queries.GetPendingItems(ctx, gen.GetPendingItemsParams{RunID: req.RunID, Column2: groupID})
	if err != nil {
		return jobstore.ClaimResult{}, err
	}
	result := jobstore.ClaimResult{Items: items, Pending: pending.PendingCount > 0}
	if pending.RetryCount > 0 {
		seconds, err := retryDelaySeconds(pending.RetryDelaySeconds)
		if err != nil {
			return jobstore.ClaimResult{}, err
		}
		next := time.Now().UTC().Add(seconds)
		result.NextRetryAt = &next
	}
	return result, nil
}

// MarkItemCompleted marks one work item done.
func (s *Store) MarkItemCompleted(ctx context.Context, rec *jobstore.SuccessRecord) error {
	if rec == nil {
		return errors.New("success record is required")
	}
	itemID, err := parseItemID(rec.ItemID)
	if err != nil {
		return err
	}
	attemptCount, err := checkedAttemptCount(rec.AttemptCount)
	if err != nil {
		return err
	}
	rows, err := s.queries.CompleteItem(ctx, gen.CompleteItemParams{
		RunID:        rec.RunID,
		ItemID:       itemID,
		AttemptCount: attemptCount,
		Column4:      rec.ResultJSON,
	})
	return expectAffected(rows, err, "complete item")
}

// MarkItemRetry schedules one work item for a later attempt.
func (s *Store) MarkItemRetry(ctx context.Context, rec *jobstore.RetryRecord) error {
	if rec == nil {
		return errors.New("retry record is required")
	}
	itemID, err := parseItemID(rec.ItemID)
	if err != nil {
		return err
	}
	attemptCount, err := checkedAttemptCount(rec.AttemptCount)
	if err != nil {
		return err
	}
	rows, err := s.queries.RetryItem(ctx, gen.RetryItemParams{
		RunID:        rec.RunID,
		ItemID:       itemID,
		AttemptCount: attemptCount,
		NextRetryAt:  timestamp(rec.RetryAt),
		LastError:    text(rec.ErrorMsg),
	})
	return expectAffected(rows, err, "retry item")
}

// MarkItemFailedFinal permanently marks one work item failed.
func (s *Store) MarkItemFailedFinal(ctx context.Context, rec jobstore.FailureRecord) error {
	itemID, err := parseItemID(rec.ItemID)
	if err != nil {
		return err
	}
	attemptCount, err := checkedAttemptCount(rec.AttemptCount)
	if err != nil {
		return err
	}
	rows, err := s.queries.FailItem(ctx, gen.FailItemParams{
		RunID:        rec.RunID,
		ItemID:       itemID,
		AttemptCount: attemptCount,
		LastError:    text(rec.ErrorMsg),
	})
	return expectAffected(rows, err, "fail item")
}

// CountItemsByState returns counts grouped by state.
func (s *Store) CountItemsByState(ctx context.Context, runID string) (map[string]int, error) {
	rows, err := s.queries.CountItemsByState(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, row := range rows {
		out[row.State] = int(row.Column2)
	}
	return out, nil
}

// RecordEvent persists a structured runtime event.
func (s *Store) RecordEvent(ctx context.Context, rec *jobstore.EventRecord) error {
	if rec == nil {
		return nil
	}
	meta := make(map[string]any, len(rec.Meta)+metadataCapacity)
	maps.Copy(meta, rec.Meta)
	if rec.Step != "" {
		meta["step"] = rec.Step
	}
	if rec.JobID != "" {
		meta["job_id"] = rec.JobID
	}
	if rec.JobKind != "" {
		meta["job_kind"] = rec.JobKind
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := s.queries.InsertEvent(ctx, gen.InsertEventParams{
		RunID:   rec.RunID,
		Level:   rec.Level,
		Kind:    rec.Kind,
		Column4: encoded,
	}); err != nil {
		slog.Warn("record_event failed", "kind", rec.Kind, "err", err)
		return err
	}
	return nil
}

// CreateRetryGroup selects failed items for a retry run.
func (s *Store) CreateRetryGroup(ctx context.Context, req *jobstore.CreateRetryGroupRequest) (jobstore.CreateRetryGroupResult, error) {
	if req == nil || req.SourceRunID == "" || req.JobID == "" || req.JobKind == "" {
		return jobstore.CreateRetryGroupResult{}, errors.New("retry group request is incomplete")
	}
	id := fmt.Sprintf("rg_%d", time.Now().UTC().UnixNano())
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return jobstore.CreateRetryGroupResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := s.queries.WithTx(tx)
	err = queries.InsertRetryGroup(ctx, gen.InsertRetryGroupParams{
		RetryGroupID: id,
		JobID:        req.JobID,
		JobKind:      req.JobKind,
		CreatedBy:    text(req.CreatedBy),
		SourceRunID:  text(req.SourceRunID),
	})
	if err != nil {
		return jobstore.CreateRetryGroupResult{}, err
	}
	var count int64
	if len(req.ItemIDs) == 0 {
		count, err = queries.TagRetryGroupAllItems(ctx, gen.TagRetryGroupAllItemsParams{RetryGroupID: text(id), RunID: req.SourceRunID})
	} else {
		ids, parseErr := parseItemIDs(req.ItemIDs)
		if parseErr != nil {
			return jobstore.CreateRetryGroupResult{}, parseErr
		}
		count, err = queries.TagRetryGroupItems(ctx, gen.TagRetryGroupItemsParams{RetryGroupID: text(id), RunID: req.SourceRunID, Column3: ids})
	}
	if err != nil {
		return jobstore.CreateRetryGroupResult{}, err
	}
	if count == 0 {
		return jobstore.CreateRetryGroupResult{}, errors.New("no failed items matched retry group criteria")
	}
	if err := tx.Commit(ctx); err != nil {
		return jobstore.CreateRetryGroupResult{}, err
	}
	return jobstore.CreateRetryGroupResult{RetryGroupID: id, ItemCount: int(count)}, nil
}

// StartRetryGroup marks a retry group running and resumes its source run.
func (s *Store) StartRetryGroup(ctx context.Context, retryGroupID string) (jobstore.RetryGroupRun, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return jobstore.RetryGroupRun{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := s.queries.WithTx(tx)
	row, err := queries.GetRetryGroupForUpdate(ctx, retryGroupID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return jobstore.RetryGroupRun{}, errRetryGroupNotFound
		}
		return jobstore.RetryGroupRun{}, err
	}
	if row.Status == "DONE" {
		return jobstore.RetryGroupRun{}, errors.New("retry group already completed")
	}
	if row.Status == "RUNNING" {
		return jobstore.RetryGroupRun{}, errors.New("retry group already running")
	}
	if rows, err := queries.MarkRetryGroupRunning(ctx, retryGroupID); err != nil {
		return jobstore.RetryGroupRun{}, err
	} else if err := expectAffected(rows, nil, "start retry group"); err != nil {
		return jobstore.RetryGroupRun{}, err
	}
	runID := row.SourceRunID.String
	if !row.SourceRunID.Valid {
		return jobstore.RetryGroupRun{}, errors.New("retry group has no source run")
	}
	updated, err := queries.ResumeRetryRun(ctx, gen.ResumeRetryRunParams{RunID: runID, JobKind: row.JobKind})
	if err != nil {
		return jobstore.RetryGroupRun{}, err
	}
	if updated == 0 {
		return jobstore.RetryGroupRun{}, errors.New("job kind already running")
	}
	if err := tx.Commit(ctx); err != nil {
		return jobstore.RetryGroupRun{}, err
	}
	return jobstore.RetryGroupRun{RetryGroupID: retryGroupID, RunID: runID, JobID: row.JobID, JobKind: row.JobKind}, nil
}

// CompleteRetryGroup marks a retry group complete.
func (s *Store) CompleteRetryGroup(ctx context.Context, retryGroupID string) error {
	rows, err := s.queries.CompleteRetryGroup(ctx, retryGroupID)
	return expectAffected(rows, err, "complete retry group")
}

// ReopenRetryGroup makes a retry group available for another attempt.
func (s *Store) ReopenRetryGroup(ctx context.Context, retryGroupID string) error {
	rows, err := s.queries.ReopenRetryGroup(ctx, retryGroupID)
	return expectAffected(rows, err, "reopen retry group")
}

// CancelRun marks a run canceled.
func (s *Store) CancelRun(ctx context.Context, runID, reason string) error {
	rows, err := s.queries.CancelRun(ctx, gen.CancelRunParams{RunID: runID, Error: text(reason)})
	return expectAffected(rows, err, "cancel run")
}

// GetRunStatus returns one run's current state.
func (s *Store) GetRunStatus(ctx context.Context, runID string) (jobstore.RunStatus, error) {
	row, err := s.queries.GetRunStatus(ctx, runID)
	if err != nil {
		return jobstore.RunStatus{}, err
	}
	return toRunStatus(&runStatusData{
		runID:         row.RunID,
		jobID:         row.JobID,
		jobKind:       row.JobKind,
		status:        row.Status,
		triggerType:   row.TriggerType,
		startedAt:     row.StartedAt.Time,
		updatedAt:     row.UpdatedAt.Time,
		currentStep:   row.CurrentStep,
		progressDone:  int(row.ProgressDone),
		progressTotal: int(row.ProgressTotal),
		errMsg:        row.Error,
	}), nil
}

// ListRunStatuses returns recent runs optionally filtered by status.
func (s *Store) ListRunStatuses(ctx context.Context, status string, limit int) ([]jobstore.RunStatus, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.queries.ListRunStatuses(ctx, gen.ListRunStatusesParams{Column1: status, Limit: sqlcLimit(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]jobstore.RunStatus, 0, len(rows))
	for i := range rows {
		row := rows[i]
		out = append(out, toRunStatus(&runStatusData{
			runID:         row.RunID,
			jobID:         row.JobID,
			jobKind:       row.JobKind,
			status:        row.Status,
			triggerType:   row.TriggerType,
			startedAt:     row.StartedAt.Time,
			updatedAt:     row.UpdatedAt.Time,
			currentStep:   row.CurrentStep,
			progressDone:  int(row.ProgressDone),
			progressTotal: int(row.ProgressTotal),
			errMsg:        row.Error,
		}))
	}
	return out, nil
}

func (s *Store) reclaimStaleRun(ctx context.Context, jobKind string) (jobstore.StartRunResult, bool, error) {
	row, err := s.queries.ReclaimStaleRunByKind(ctx, jobKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobstore.StartRunResult{}, false, nil
	}
	if err != nil {
		return jobstore.StartRunResult{}, false, fmt.Errorf("reclaim stale run: %w", err)
	}
	return jobstore.StartRunResult{RunID: row.RunID, SlotTS: row.SlotTs.Time}, true, nil
}

func sqlcLimit(value int) int32 {
	if int64(value) > maxSQLCLimit {
		return int32(maxSQLCLimit)
	}
	return int32(value) //nolint:gosec // values are checked by the caller and bounded above
}

func (s *Store) runningRun(ctx context.Context, jobKind string) (jobstore.StartRunResult, bool, error) {
	row, err := s.queries.GetRunningRunByKind(ctx, jobKind)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return jobstore.StartRunResult{}, false, nil
		}
		return jobstore.StartRunResult{}, false, err
	}
	return jobstore.StartRunResult{RunID: row.RunID, SlotTS: row.SlotTs.Time, ExistingRunning: true}, true, nil
}

func parseItemID(itemID jobstore.ItemID) (int64, error) {
	return strconv.ParseInt(string(itemID), 10, 64)
}

func parseItemIDs(itemIDs []jobstore.ItemID) ([]int64, error) {
	values := make([]int64, 0, len(itemIDs))
	for _, itemID := range itemIDs {
		value, err := parseItemID(itemID)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func timestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

func text(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

func expectAffected(rows int64, err error, operation string) error {
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%s: %w", operation, jobstore.ErrNotFound)
	}
	return nil
}

func checkedAttemptCount(value int) (int32, error) {
	if value < 0 || int64(value) > maxAttemptCount {
		return 0, fmt.Errorf("invalid attempt count %d", value)
	}
	return int32(value), nil
}

func retryDelaySeconds(value any) (time.Duration, error) {
	var seconds int64
	switch typed := value.(type) {
	case int64:
		seconds = typed
	case int32:
		seconds = int64(typed)
	case int:
		seconds = int64(typed)
	case float64:
		seconds = int64(typed)
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse retry delay: %w", err)
		}
		seconds = parsed
	default:
		return 0, fmt.Errorf("parse retry delay: unsupported type %T", value)
	}
	if seconds < 0 {
		return 0, errors.New("retry delay must not be negative")
	}
	return time.Duration(seconds) * time.Second, nil
}

type runStatusData struct {
	runID         string
	jobID         string
	jobKind       string
	status        string
	triggerType   string
	startedAt     time.Time
	updatedAt     time.Time
	currentStep   string
	progressDone  int
	progressTotal int
	errMsg        string
}

func toRunStatus(data *runStatusData) jobstore.RunStatus {
	return jobstore.RunStatus{
		RunID:         data.runID,
		JobID:         data.jobID,
		JobKind:       data.jobKind,
		Status:        data.status,
		TriggerType:   data.triggerType,
		StartedAt:     data.startedAt,
		UpdatedAt:     data.updatedAt,
		CurrentStep:   data.currentStep,
		ProgressDone:  data.progressDone,
		ProgressTotal: data.progressTotal,
		Error:         data.errMsg,
	}
}

var _ jobstore.Store = (*Store)(nil)
