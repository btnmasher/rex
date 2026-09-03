// Package sqlite provides a process-local durable job store backed by SQLite.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/btnmasher/rex/jobruntime/jobstore"
	"github.com/btnmasher/rex/jobruntime/jobstore/sqlite/gen"
	_ "modernc.org/sqlite"
)

const (
	defaultClaimSize = 100
	metadataCapacity = 3
	maxAttemptCount  = int64(^uint32(0) >> 1)
	maxInt           = int64(^uint(0) >> 1)
)

var errRetryGroupNotFound = errors.New("retry group not found")

// Store implements jobstore.Store with SQLC-generated SQLite queries.
// SQLite is intended for one service process; the store uses one connection to
// keep transaction and in-memory database behavior deterministic.
type Store struct {
	db      *sql.DB
	queries *gen.Queries
}

var _ jobstore.Store = (*Store)(nil)

// NewStore opens or creates a SQLite database at path and applies pending migrations using ctx.
// Use ":memory:" for an ephemeral database that still exercises durable-store behavior.
func NewStore(ctx context.Context, path string) (*Store, error) {
	if ctx == nil {
		return nil, errors.New("sqlite database context is required")
	}
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("sqlite database path is required")
	}
	dsn := withDefaults(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite database: %w", err)
	}
	if err := applyMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate sqlite schema: %w", err)
	}
	return &Store{db: db, queries: gen.New(db)}, nil
}

// Close releases the SQLite database connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
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
		RunID: runID, JobID: req.JobID, JobKind: req.JobKind,
		SlotTs: now.Unix(), TriggerType: req.TriggerType, TriggerMeta: meta,
	})
	if err != nil {
		if existing, ok, lookupErr := s.runningRun(ctx, req.JobKind); lookupErr == nil && ok {
			return existing, nil
		}
		return jobstore.StartRunResult{}, fmt.Errorf("insert run: %w", err)
	}
	_ = s.RecordEvent(ctx, &jobstore.EventRecord{RunID: row.RunID, Level: "INFO", Kind: "RUN_STARTED", JobID: req.JobID, JobKind: req.JobKind})
	return jobstore.StartRunResult{RunID: row.RunID, SlotTS: fromUnix(row.SlotTs)}, nil
}

// MarkRunCompleted marks a run complete and persists its statistics.
func (s *Store) MarkRunCompleted(ctx context.Context, runID string, stats map[string]any) error {
	statsJSON, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("marshal run statistics: %w", err)
	}
	rows, err := s.queries.CompleteRun(ctx, gen.CompleteRunParams{RunID: runID, StatsJson: statsJSON})
	return expectAffected(rows, err, "complete run")
}

// MarkRunFailed marks a run failed with an operator-visible message.
func (s *Store) MarkRunFailed(ctx context.Context, runID, errMsg string) error {
	rows, err := s.queries.FailRun(ctx, gen.FailRunParams{RunID: runID, Error: text(errMsg)})
	return expectAffected(rows, err, "fail run")
}

// Heartbeat updates the run's last activity timestamp.
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
	return jobstore.Checkpoint{
		Namespace: row.CheckpointNamespace,
		Value:     append([]byte(nil), row.CheckpointValue...),
	}, nil
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin insert items transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	for _, item := range items {
		itemID, err := parseItemID(item.ItemID)
		if err != nil {
			return err
		}
		rows, err := queries.InsertWorkItem(ctx, gen.InsertWorkItemParams{
			RunID:       runID,
			ItemID:      itemID,
			PayloadJson: item.PayloadJSON,
			ResultJson:  item.ResultJSON,
		})
		if err != nil {
			return err
		}
		if rows == 0 {
			return fmt.Errorf("insert work item %s: existing payload differs", item.ItemID)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit insert items transaction: %w", err)
	}
	return nil
}

// ClaimItems atomically claims ready work in deterministic ID order.
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
		Limit:   int64(limit),
	})
	if err != nil {
		return jobstore.ClaimResult{}, err
	}
	items := make([]jobstore.WorkItem, 0, len(rows))
	for _, row := range rows {
		attempt, err := checkedInt(row.AttemptCount)
		if err != nil {
			return jobstore.ClaimResult{}, err
		}
		items = append(items, jobstore.WorkItem{
			RunID:        row.RunID,
			ItemID:       jobstore.ItemID(strconv.FormatInt(row.ItemID, 10)),
			AttemptCount: attempt,
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
	rows, err := s.queries.CompleteItem(ctx, gen.CompleteItemParams{
		RunID:        rec.RunID,
		ItemID:       itemID,
		AttemptCount: int64(rec.AttemptCount),
		ResultJson:   rec.ResultJSON,
	})
	return expectAffected(rows, err, "complete item")
}

// MarkItemRetry schedules a failed work item for another attempt.
func (s *Store) MarkItemRetry(ctx context.Context, rec *jobstore.RetryRecord) error {
	if rec == nil {
		return errors.New("retry record is required")
	}
	itemID, err := parseItemID(rec.ItemID)
	if err != nil {
		return err
	}
	rows, err := s.queries.RetryItem(ctx, gen.RetryItemParams{
		RunID:        rec.RunID,
		ItemID:       itemID,
		AttemptCount: int64(rec.AttemptCount),
		NextRetryAt:  unixTime(rec.RetryAt),
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
	rows, err := s.queries.FailItem(ctx, gen.FailItemParams{
		RunID:        rec.RunID,
		ItemID:       itemID,
		AttemptCount: int64(rec.AttemptCount),
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
		value, err := checkedInt(row.Count)
		if err != nil {
			return nil, err
		}
		out[row.State] = value
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
		RunID: rec.RunID,
		Level: rec.Level,
		Kind:  rec.Kind,
		Meta:  encoded,
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return jobstore.CreateRetryGroupResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	if err := queries.InsertRetryGroup(ctx, gen.InsertRetryGroupParams{
		RetryGroupID: id,
		JobID:        req.JobID,
		JobKind:      req.JobKind,
		CreatedBy:    text(req.CreatedBy),
		SourceRunID:  text(req.SourceRunID),
	}); err != nil {
		return jobstore.CreateRetryGroupResult{}, err
	}
	var count int64
	if len(req.ItemIDs) == 0 {
		count, err = queries.TagRetryGroupAllItems(ctx, gen.TagRetryGroupAllItemsParams{RetryGroupID: text(id), RunID: req.SourceRunID})
	} else {
		for _, itemID := range req.ItemIDs {
			parsed, parseErr := parseItemID(itemID)
			if parseErr != nil {
				return jobstore.CreateRetryGroupResult{}, parseErr
			}
			updated, updateErr := queries.TagRetryGroupItem(ctx, gen.TagRetryGroupItemParams{
				RetryGroupID: text(id),
				RunID:        req.SourceRunID,
				ItemID:       parsed,
			})
			if updateErr != nil {
				err = updateErr
				break
			}
			count += updated
		}
	}
	if err != nil {
		return jobstore.CreateRetryGroupResult{}, err
	}
	if count == 0 {
		return jobstore.CreateRetryGroupResult{}, errors.New("no failed items matched retry group criteria")
	}
	if err := tx.Commit(); err != nil {
		return jobstore.CreateRetryGroupResult{}, err
	}
	return jobstore.CreateRetryGroupResult{RetryGroupID: id, ItemCount: int(count)}, nil
}

// StartRetryGroup marks a retry group running and resumes its source run.
func (s *Store) StartRetryGroup(ctx context.Context, retryGroupID string) (jobstore.RetryGroupRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return jobstore.RetryGroupRun{}, err
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	row, err := queries.GetRetryGroup(ctx, retryGroupID)
	if errors.Is(err, sql.ErrNoRows) {
		return jobstore.RetryGroupRun{}, errRetryGroupNotFound
	}
	if err != nil {
		return jobstore.RetryGroupRun{}, err
	}
	if row.Status == "DONE" {
		return jobstore.RetryGroupRun{}, errors.New("retry group already completed")
	}
	if row.Status == "RUNNING" {
		return jobstore.RetryGroupRun{}, errors.New("retry group already running")
	}
	rows, err := queries.MarkRetryGroupRunning(ctx, retryGroupID)
	if err != nil {
		return jobstore.RetryGroupRun{}, err
	}
	if err := expectAffected(rows, nil, "start retry group"); err != nil {
		return jobstore.RetryGroupRun{}, err
	}
	if !row.SourceRunID.Valid {
		return jobstore.RetryGroupRun{}, errors.New("retry group has no source run")
	}
	updated, err := queries.ResumeRetryRun(ctx, gen.ResumeRetryRunParams{RunID: row.SourceRunID.String, JobKind: row.JobKind})
	if err != nil {
		return jobstore.RetryGroupRun{}, err
	}
	if updated == 0 {
		return jobstore.RetryGroupRun{}, errors.New("job kind already running")
	}
	if err := tx.Commit(); err != nil {
		return jobstore.RetryGroupRun{}, err
	}
	return jobstore.RetryGroupRun{RetryGroupID: retryGroupID, RunID: row.SourceRunID.String, JobID: row.JobID, JobKind: row.JobKind}, nil
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
	done, err := checkedInt(row.ProgressDone)
	if err != nil {
		return jobstore.RunStatus{}, err
	}
	total, err := checkedInt(row.ProgressTotal)
	if err != nil {
		return jobstore.RunStatus{}, err
	}
	return toRunStatus(&runStatusData{
		runID:         row.RunID,
		jobID:         row.JobID,
		jobKind:       row.JobKind,
		status:        row.Status,
		triggerType:   row.TriggerType,
		startedAt:     fromUnix(row.StartedAt),
		updatedAt:     fromUnix(row.UpdatedAt),
		currentStep:   row.CurrentStep,
		progressDone:  done,
		progressTotal: total,
		errMsg:        row.Error,
	}), nil
}

// ListRunStatuses returns recent runs optionally filtered by status.
func (s *Store) ListRunStatuses(ctx context.Context, status string, limit int) ([]jobstore.RunStatus, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.queries.ListRunStatuses(ctx, gen.ListRunStatusesParams{Column1: status, Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]jobstore.RunStatus, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		done, err := checkedInt(row.ProgressDone)
		if err != nil {
			return nil, err
		}
		total, err := checkedInt(row.ProgressTotal)
		if err != nil {
			return nil, err
		}
		out = append(out, toRunStatus(&runStatusData{
			runID:         row.RunID,
			jobID:         row.JobID,
			jobKind:       row.JobKind,
			status:        row.Status,
			triggerType:   row.TriggerType,
			startedAt:     fromUnix(row.StartedAt),
			updatedAt:     fromUnix(row.UpdatedAt),
			currentStep:   row.CurrentStep,
			progressDone:  done,
			progressTotal: total,
			errMsg:        row.Error,
		}))
	}
	return out, nil
}

func (s *Store) reclaimStaleRun(ctx context.Context, jobKind string) (jobstore.StartRunResult, bool, error) {
	row, err := s.queries.ReclaimStaleRunByKind(ctx, jobKind)
	if errors.Is(err, sql.ErrNoRows) {
		return jobstore.StartRunResult{}, false, nil
	}
	if err != nil {
		return jobstore.StartRunResult{}, false, fmt.Errorf("reclaim stale run: %w", err)
	}
	return jobstore.StartRunResult{RunID: row.RunID, SlotTS: fromUnix(row.SlotTs)}, true, nil
}

func (s *Store) runningRun(ctx context.Context, jobKind string) (jobstore.StartRunResult, bool, error) {
	row, err := s.queries.GetRunningRunByKind(ctx, jobKind)
	if errors.Is(err, sql.ErrNoRows) {
		return jobstore.StartRunResult{}, false, nil
	}
	if err != nil {
		return jobstore.StartRunResult{}, false, err
	}
	return jobstore.StartRunResult{RunID: row.RunID, SlotTS: fromUnix(row.SlotTs), ExistingRunning: true}, true, nil
}

func withDefaults(path string) string {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + "_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
}

func fromUnix(value int64) time.Time { return time.Unix(value, 0).UTC() }

func unixTime(value time.Time) sql.NullInt64 {
	return sql.NullInt64{Int64: value.Unix(), Valid: !value.IsZero()}
}

func parseItemID(itemID jobstore.ItemID) (int64, error) {
	return strconv.ParseInt(string(itemID), 10, 64)
}

func text(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
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

func checkedInt(value int64) (int, error) {
	if value < 0 || value > maxInt {
		return 0, fmt.Errorf("integer value %d is out of range", value)
	}
	return int(value), nil
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
	case []byte:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse retry delay: %w", err)
		}
		seconds = parsed
	default:
		return 0, fmt.Errorf("unsupported retry delay type %T", value)
	}
	if seconds < 0 {
		seconds = 0
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
