// Package memory provides an ephemeral job store for single-process runners.
package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/btnmasher/rex/jobruntime/jobstore"
)

var errNotFound = jobstore.ErrNotFound

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	return ctx.Err()
}

const (
	staleRunTimeout = 5 * time.Minute
	inflightTimeout = 15 * time.Minute
)

type run struct {
	status     jobstore.RunStatus
	slot       time.Time
	checkpoint jobstore.Checkpoint
	items      map[jobstore.ItemID]*item
}

type item struct {
	work       jobstore.WorkItem
	state      string
	nextRetry  time.Time
	retryGroup string
	lastError  string
	updatedAt  time.Time
}

type retryGroup struct {
	jobID       string
	jobKind     string
	sourceRunID string
	status      string
}

// Store is a mutex-protected, process-local implementation of jobstore.Store.
type Store struct {
	mu          sync.Mutex
	runSequence uint64
	runs        map[string]*run
	retryGroups map[string]*retryGroup
	events      []jobstore.EventRecord
}

var _ jobstore.Store = (*Store)(nil)

// NewStore creates an empty ephemeral store.
func NewStore() *Store {
	return &Store{
		runs:        make(map[string]*run),
		retryGroups: make(map[string]*retryGroup),
	}
}

// Close releases no external resources and exists for interchangeable wiring.
func (s *Store) Close() {}

// StartOrGetRun creates a run unless a run of the same kind is active.
func (s *Store) StartOrGetRun(ctx context.Context, req *jobstore.StartRunRequest) (jobstore.StartRunResult, error) {
	if err := contextErr(ctx); err != nil {
		return jobstore.StartRunResult{}, err
	}
	if req == nil || req.JobKind == "" {
		return jobstore.StartRunResult{}, errors.New("job kind is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for id, existing := range s.runs {
		if existing.status.Status == "RUNNING" && existing.status.JobKind == req.JobKind {
			if existing.status.UpdatedAt.Add(staleRunTimeout).Before(now) {
				existing.status.UpdatedAt = now
				existing.status.Error = ""
				return jobstore.StartRunResult{RunID: id, SlotTS: existing.slot}, nil
			} else {
				return jobstore.StartRunResult{RunID: id, SlotTS: existing.slot, ExistingRunning: true}, nil
			}
		}
	}

	s.runSequence++
	runID := fmt.Sprintf("%s:%s:%d", req.JobID, req.JobKind, s.runSequence)
	s.runs[runID] = &run{
		slot: now,
		status: jobstore.RunStatus{
			RunID:       runID,
			JobID:       req.JobID,
			JobKind:     req.JobKind,
			Status:      "RUNNING",
			TriggerType: req.TriggerType,
			StartedAt:   now,
			UpdatedAt:   now,
		},
		items: make(map[jobstore.ItemID]*item),
	}
	return jobstore.StartRunResult{RunID: runID, SlotTS: now}, nil
}

// MarkRunCompleted marks a run successful and stores its JSON-compatible stats.
func (s *Store) MarkRunCompleted(ctx context.Context, runID string, stats map[string]any) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.getRun(runID)
	if err != nil {
		return err
	}
	if r.status.Status != "RUNNING" {
		return fmt.Errorf("complete run %s: run is not running", runID)
	}
	r.status.Status = "COMPLETED"
	r.status.UpdatedAt = time.Now().UTC()
	return setStats(&r.status, stats)
}

// MarkRunFailed marks a run failed with an operator-visible message.
func (s *Store) MarkRunFailed(ctx context.Context, runID, errMsg string) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.getRun(runID)
	if err != nil {
		return err
	}
	if r.status.Status != "RUNNING" {
		return fmt.Errorf("fail run %s: run is not running", runID)
	}
	r.status.Status = "FAILED"
	r.status.Error = errMsg
	r.status.UpdatedAt = time.Now().UTC()
	return nil
}

// Heartbeat updates the run's last activity timestamp.
func (s *Store) Heartbeat(ctx context.Context, runID string) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.getRun(runID)
	if err != nil {
		return err
	}
	if r.status.Status != "RUNNING" {
		return fmt.Errorf("heartbeat run %s: run is not running", runID)
	}
	r.status.UpdatedAt = time.Now().UTC()
	return nil
}

// GetRunCheckpoint returns opaque producer progress for a run.
func (s *Store) GetRunCheckpoint(ctx context.Context, runID string) (jobstore.Checkpoint, error) {
	if err := contextErr(ctx); err != nil {
		return jobstore.Checkpoint{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.getRun(runID)
	if err != nil {
		return jobstore.Checkpoint{}, err
	}
	return cloneCheckpoint(r.checkpoint), nil
}

// SaveRunCheckpoint replaces the producer checkpoint for a run.
func (s *Store) SaveRunCheckpoint(ctx context.Context, runID string, checkpoint jobstore.Checkpoint) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.getRun(runID)
	if err != nil {
		return err
	}
	r.checkpoint = cloneCheckpoint(checkpoint)
	return nil
}

func cloneCheckpoint(checkpoint jobstore.Checkpoint) jobstore.Checkpoint {
	checkpoint.Value = append([]byte(nil), checkpoint.Value...)
	return checkpoint
}

// InsertItems adds ready work items to a run.
func (s *Store) InsertItems(ctx context.Context, runID string, items []jobstore.WorkItem) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.getRun(runID)
	if err != nil {
		return err
	}
	if err := validateWorkItems(r, items); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, work := range items {
		if _, exists := r.items[work.ItemID]; exists {
			continue
		}
		work.RunID = runID
		work.PayloadJSON = append([]byte(nil), work.PayloadJSON...)
		work.ResultJSON = append([]byte(nil), work.ResultJSON...)
		r.items[work.ItemID] = &item{work: work, state: "READY", updatedAt: now}
	}
	return nil
}

func validateWorkItems(r *run, items []jobstore.WorkItem) error {
	seen := make(map[jobstore.ItemID][]byte, len(items))
	for _, work := range items {
		if work.ItemID == "" {
			return errors.New("work item ID is required")
		}
		if existing, exists := r.items[work.ItemID]; exists {
			if !bytes.Equal(existing.work.PayloadJSON, work.PayloadJSON) {
				return fmt.Errorf("work item %s already exists with different payload", work.ItemID)
			}
			continue
		}
		if previous, exists := seen[work.ItemID]; exists && !bytes.Equal(previous, work.PayloadJSON) {
			return fmt.Errorf("work item %s appears more than once with different payload", work.ItemID)
		}
		seen[work.ItemID] = work.PayloadJSON
	}
	return nil
}

// ClaimItems atomically claims ready items in deterministic ID order.
func (s *Store) ClaimItems(ctx context.Context, req jobstore.ClaimRequest) (jobstore.ClaimResult, error) {
	if err := contextErr(ctx); err != nil {
		return jobstore.ClaimResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.getRun(req.RunID)
	if err != nil {
		return jobstore.ClaimResult{}, err
	}
	limit := req.BatchSize
	if limit <= 0 {
		limit = 100
	}
	now := time.Now().UTC()
	claimed := claimItems(r, req, limit, now)
	result := pendingResult(r.items, req.RetryGroupID)
	result.Items = claimed
	return result, nil
}

func claimItems(r *run, req jobstore.ClaimRequest, limit int, now time.Time) []jobstore.WorkItem {
	ids := make([]string, 0, len(r.items))
	for id := range r.items {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	claimed := make([]jobstore.WorkItem, 0, limit)
	for _, rawID := range ids {
		if len(claimed) >= limit {
			break
		}
		entry := r.items[jobstore.ItemID(rawID)]
		if !claimable(entry, now) {
			continue
		}
		if !entry.nextRetry.IsZero() && entry.nextRetry.After(now) {
			continue
		}
		if !matchesRetryGroup(entry, req.RetryGroupID) {
			continue
		}
		entry.state = "INFLIGHT"
		entry.work.AttemptCount++
		entry.updatedAt = now
		claimed = append(claimed, entry.work)
	}
	return claimed
}

func pendingResult(items map[jobstore.ItemID]*item, retryGroupID *string) jobstore.ClaimResult {
	result := jobstore.ClaimResult{}
	for _, entry := range items {
		if !matchesRetryGroup(entry, retryGroupID) {
			continue
		}
		if entry.state != "READY" && entry.state != "FAILED_RETRY" && entry.state != "INFLIGHT" {
			continue
		}
		result.Pending = true
		if entry.state != "FAILED_RETRY" || entry.nextRetry.IsZero() {
			continue
		}
		if result.NextRetryAt == nil || entry.nextRetry.Before(*result.NextRetryAt) {
			next := entry.nextRetry
			result.NextRetryAt = &next
		}
	}
	return result
}

func claimable(entry *item, now time.Time) bool {
	ready := entry.state == "READY" || entry.state == "FAILED_RETRY"
	stale := entry.state == "INFLIGHT" && entry.updatedAt.Add(inflightTimeout).Before(now)
	return ready || stale
}

func matchesRetryGroup(entry *item, retryGroupID *string) bool {
	return retryGroupID == nil || entry.retryGroup == *retryGroupID
}

// MarkItemCompleted marks a work item done.
func (s *Store) MarkItemCompleted(ctx context.Context, rec *jobstore.SuccessRecord) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if rec == nil {
		return errors.New("success record is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, err := s.getItem(rec.RunID, rec.ItemID)
	if err != nil {
		return err
	}
	if entry.state != "INFLIGHT" {
		return fmt.Errorf("complete item %s: item is not inflight", rec.ItemID)
	}
	if entry.work.AttemptCount != rec.AttemptCount {
		return fmt.Errorf("complete item %s: %w", rec.ItemID, jobstore.ErrStaleClaim)
	}
	entry.state = "DONE"
	entry.work.ResultJSON = append([]byte(nil), rec.ResultJSON...)
	entry.nextRetry = time.Time{}
	entry.retryGroup = ""
	entry.lastError = ""
	entry.updatedAt = time.Now().UTC()
	return nil
}

// MarkItemRetry schedules a failed work item for another attempt.
func (s *Store) MarkItemRetry(ctx context.Context, rec *jobstore.RetryRecord) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if rec == nil {
		return errors.New("retry record is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, err := s.getItem(rec.RunID, rec.ItemID)
	if err != nil {
		return err
	}
	if entry.state != "INFLIGHT" {
		return fmt.Errorf("retry item %s: item is not inflight", rec.ItemID)
	}
	if entry.work.AttemptCount != rec.AttemptCount {
		return fmt.Errorf("retry item %s: %w", rec.ItemID, jobstore.ErrStaleClaim)
	}
	entry.state = "FAILED_RETRY"
	entry.nextRetry = rec.RetryAt
	entry.lastError = rec.ErrorMsg
	entry.updatedAt = time.Now().UTC()
	return nil
}

// MarkItemFailedFinal permanently marks a work item failed.
func (s *Store) MarkItemFailedFinal(ctx context.Context, rec jobstore.FailureRecord) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, err := s.getItem(rec.RunID, rec.ItemID)
	if err != nil {
		return err
	}
	if entry.state != "INFLIGHT" {
		return fmt.Errorf("fail item %s: item is not inflight", rec.ItemID)
	}
	if entry.work.AttemptCount != rec.AttemptCount {
		return fmt.Errorf("fail item %s: %w", rec.ItemID, jobstore.ErrStaleClaim)
	}
	entry.state = "FAILED_FINAL"
	entry.lastError = rec.ErrorMsg
	entry.updatedAt = time.Now().UTC()
	return nil
}

// CountItemsByState returns counts grouped by item state.
func (s *Store) CountItemsByState(ctx context.Context, runID string) (map[string]int, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.getRun(runID)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int)
	for _, entry := range r.items {
		counts[entry.state]++
	}
	return counts, nil
}

// RecordEvent appends an event to the process-local event history.
func (s *Store) RecordEvent(ctx context.Context, rec *jobstore.EventRecord) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec != nil {
		s.events = append(s.events, *rec)
	}
	return nil
}

// CreateRetryGroup tags failed items for a later retry run.
func (s *Store) CreateRetryGroup(ctx context.Context, req *jobstore.CreateRetryGroupRequest) (jobstore.CreateRetryGroupResult, error) {
	if err := contextErr(ctx); err != nil {
		return jobstore.CreateRetryGroupResult{}, err
	}
	if req == nil || req.SourceRunID == "" || req.JobID == "" || req.JobKind == "" {
		return jobstore.CreateRetryGroupResult{}, errors.New("retry group request is incomplete")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.getRun(req.SourceRunID); err != nil {
		return jobstore.CreateRetryGroupResult{}, err
	}
	id := fmt.Sprintf("rg_%d", time.Now().UTC().UnixNano())
	s.retryGroups[id] = &retryGroup{jobID: req.JobID, jobKind: req.JobKind, sourceRunID: req.SourceRunID, status: "OPEN"}
	targets := make(map[jobstore.ItemID]struct{}, len(req.ItemIDs))
	for _, itemID := range req.ItemIDs {
		targets[itemID] = struct{}{}
	}
	count := 0
	for itemID, entry := range s.runs[req.SourceRunID].items {
		if len(targets) > 0 {
			if _, ok := targets[itemID]; !ok {
				continue
			}
		}
		if entry.state != "FAILED_FINAL" && entry.state != "FAILED_RETRY" {
			continue
		}
		entry.retryGroup = id
		entry.state = "READY"
		entry.nextRetry = time.Time{}
		entry.lastError = ""
		count++
	}
	if count == 0 {
		delete(s.retryGroups, id)
		return jobstore.CreateRetryGroupResult{}, errors.New("no failed items matched retry group criteria")
	}
	return jobstore.CreateRetryGroupResult{RetryGroupID: id, ItemCount: count}, nil
}

// StartRetryGroup reopens its source run for processing if no same-kind run is active.
func (s *Store) StartRetryGroup(ctx context.Context, retryGroupID string) (jobstore.RetryGroupRun, error) {
	if err := contextErr(ctx); err != nil {
		return jobstore.RetryGroupRun{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	group, ok := s.retryGroups[retryGroupID]
	if !ok {
		return jobstore.RetryGroupRun{}, fmt.Errorf("%w: retry group %s", errNotFound, retryGroupID)
	}
	if group.status == "DONE" {
		return jobstore.RetryGroupRun{}, errors.New("retry group already completed")
	}
	if group.status == "RUNNING" {
		return jobstore.RetryGroupRun{}, errors.New("retry group already running")
	}
	for _, existing := range s.runs {
		if existing.status.Status == "RUNNING" && existing.status.JobKind == group.jobKind {
			return jobstore.RetryGroupRun{}, errors.New("job kind already running")
		}
	}
	r, err := s.getRun(group.sourceRunID)
	if err != nil {
		return jobstore.RetryGroupRun{}, err
	}
	r.status.Status = "RUNNING"
	group.status = "RUNNING"
	return jobstore.RetryGroupRun{RetryGroupID: retryGroupID, RunID: group.sourceRunID, JobID: group.jobID, JobKind: group.jobKind}, nil
}

// CompleteRetryGroup marks a retry group complete.
func (s *Store) CompleteRetryGroup(ctx context.Context, retryGroupID string) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	group, ok := s.retryGroups[retryGroupID]
	if !ok {
		return errNotFound
	}
	group.status = "DONE"
	return nil
}

// ReopenRetryGroup makes a failed retry group runnable again.
func (s *Store) ReopenRetryGroup(ctx context.Context, retryGroupID string) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	group, ok := s.retryGroups[retryGroupID]
	if !ok {
		return errNotFound
	}
	group.status = "OPEN"
	return nil
}

// CancelRun marks a run canceled.
func (s *Store) CancelRun(ctx context.Context, runID, reason string) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.getRun(runID)
	if err != nil {
		return err
	}
	if r.status.Status != "RUNNING" {
		return fmt.Errorf("cancel run %s: run is not running", runID)
	}
	r.status.Status = "CANCELED"
	r.status.Error = reason
	r.status.UpdatedAt = time.Now().UTC()
	return nil
}

// GetRunStatus returns one run's current status.
func (s *Store) GetRunStatus(ctx context.Context, runID string) (jobstore.RunStatus, error) {
	if err := contextErr(ctx); err != nil {
		return jobstore.RunStatus{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.getRun(runID)
	if err != nil {
		return jobstore.RunStatus{}, err
	}
	return r.status, nil
}

// ListRunStatuses returns recent runs optionally filtered by status.
func (s *Store) ListRunStatuses(ctx context.Context, status string, limit int) ([]jobstore.RunStatus, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	out := make([]jobstore.RunStatus, 0, len(s.runs))
	for _, r := range s.runs {
		if status != "" && r.status.Status != status {
			continue
		}
		out = append(out, r.status)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) getRun(runID string) (*run, error) {
	r, ok := s.runs[runID]
	if !ok {
		return nil, fmt.Errorf("%w: run %s", errNotFound, runID)
	}
	return r, nil
}

func (s *Store) getItem(runID string, itemID jobstore.ItemID) (*item, error) {
	r, err := s.getRun(runID)
	if err != nil {
		return nil, err
	}
	entry, ok := r.items[itemID]
	if !ok {
		return nil, fmt.Errorf("%w: item %s", errNotFound, itemID)
	}
	return entry, nil
}

func setStats(status *jobstore.RunStatus, stats map[string]any) error {
	if len(stats) == 0 {
		return nil
	}
	encoded, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	var values map[string]any
	if err := json.Unmarshal(encoded, &values); err != nil {
		return err
	}
	if value, ok := values["progress_done"].(float64); ok {
		status.ProgressDone = int(value)
	}
	if value, ok := values["progress_total"].(float64); ok {
		status.ProgressTotal = int(value)
	}
	return nil
}

var _ jobstore.Store = (*Store)(nil)
