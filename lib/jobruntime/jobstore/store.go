package jobstore

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound indicates that a requested runtime record does not exist.
var ErrNotFound = errors.New("job runtime record not found")

// ErrStaleClaim indicates that a worker tried to mutate an item after its claim was reclaimed.
var ErrStaleClaim = errors.New("job runtime claim is stale")

// ItemID identifies one work item within a run.
type ItemID string

// WorkItem is the store representation of one claimed item.
type WorkItem struct {
	RunID        string
	ItemID       ItemID
	AttemptCount int
	PayloadJSON  []byte
	ResultJSON   []byte
}

// StartRunRequest describes a new or resumable job run.
type StartRunRequest struct {
	JobID         string
	JobKind       string
	TriggerType   string
	TriggerEntity string
	TriggerMeta   map[string]any
	RetryGroupID  *string
}

// StartRunResult describes the run selected by the store.
type StartRunResult struct {
	RunID           string
	SlotTS          time.Time
	ExistingRunning bool
}

// Checkpoint is opaque producer progress persisted with a run.
// Namespace identifies the checkpoint format, while Value contains its encoded value.
type Checkpoint struct {
	Namespace string
	Value     []byte
}

// Empty reports whether the checkpoint has no persisted value.
func (c Checkpoint) Empty() bool {
	return c.Namespace == "" && len(c.Value) == 0
}

// Validate checks that a non-empty checkpoint has both a namespace and value.
func (c Checkpoint) Validate() error {
	if c.Empty() {
		return nil
	}
	if c.Namespace == "" {
		return errors.New("checkpoint namespace is required")
	}
	if len(c.Value) == 0 {
		return errors.New("checkpoint value is required")
	}
	return nil
}

// ClaimRequest identifies the work items a worker wants to claim.
type ClaimRequest struct {
	RunID        string
	RetryGroupID *string
	BatchSize    int
}

// ClaimResult contains claimed items and whether more work remains.
type ClaimResult struct {
	Items       []WorkItem
	Pending     bool
	NextRetryAt *time.Time
}

// SuccessRecord records a completed work item.
type SuccessRecord struct {
	RunID        string
	ItemID       ItemID
	AttemptCount int
	ResultJSON   []byte
}

// RetryRecord records a retryable work-item failure.
type RetryRecord struct {
	RunID        string
	ItemID       ItemID
	AttemptCount int
	Attempt      int
	RetryAt      time.Time
	ErrorMsg     string
}

// FailureRecord records a terminal work-item failure.
type FailureRecord struct {
	RunID        string
	ItemID       ItemID
	AttemptCount int
	ErrorMsg     string
}

// EventRecord records structured runtime diagnostics.
type EventRecord struct {
	RunID   string
	Level   string
	Kind    string
	Meta    map[string]any
	Step    string
	JobID   string
	JobKind string
}

// CreateRetryGroupRequest selects failed items for a retry run.
type CreateRetryGroupRequest struct {
	JobID       string
	JobKind     string
	SourceRunID string
	CreatedBy   string
	ItemIDs     []ItemID
}

// CreateRetryGroupResult reports the created retry group.
type CreateRetryGroupResult struct {
	RetryGroupID string
	ItemCount    int
}

// RetryGroupRun identifies the run resumed by a retry group.
type RetryGroupRun struct {
	RetryGroupID string
	RunID        string
	JobID        string
	JobKind      string
}

// RunStatus is the operator-visible state of a job run.
type RunStatus struct {
	RunID         string    `json:"run_id"`
	JobID         string    `json:"job_id"`
	JobKind       string    `json:"job_kind"`
	Status        string    `json:"status"`
	TriggerType   string    `json:"trigger_type"`
	StartedAt     time.Time `json:"started_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	CurrentStep   string    `json:"current_step"`
	ProgressDone  int       `json:"progress_done"`
	ProgressTotal int       `json:"progress_total"`
	Error         string    `json:"error"`
}

// Store persists and claims job runtime state.
type Store interface {
	StartOrGetRun(ctx context.Context, req *StartRunRequest) (StartRunResult, error)
	MarkRunCompleted(ctx context.Context, runID string, stats map[string]any) error
	MarkRunFailed(ctx context.Context, runID, errMsg string) error
	Heartbeat(ctx context.Context, runID string) error
	GetRunCheckpoint(ctx context.Context, runID string) (Checkpoint, error)
	SaveRunCheckpoint(ctx context.Context, runID string, checkpoint Checkpoint) error
	InsertItems(ctx context.Context, runID string, items []WorkItem) error
	ClaimItems(ctx context.Context, req ClaimRequest) (ClaimResult, error)
	MarkItemCompleted(ctx context.Context, rec *SuccessRecord) error
	MarkItemRetry(ctx context.Context, rec *RetryRecord) error
	MarkItemFailedFinal(ctx context.Context, rec FailureRecord) error
	CountItemsByState(ctx context.Context, runID string) (map[string]int, error)
	RecordEvent(ctx context.Context, rec *EventRecord) error
	CreateRetryGroup(ctx context.Context, req *CreateRetryGroupRequest) (CreateRetryGroupResult, error)
	StartRetryGroup(ctx context.Context, retryGroupID string) (RetryGroupRun, error)
	CompleteRetryGroup(ctx context.Context, retryGroupID string) error
	ReopenRetryGroup(ctx context.Context, retryGroupID string) error
	CancelRun(ctx context.Context, runID, reason string) error
	GetRunStatus(ctx context.Context, runID string) (RunStatus, error)
	ListRunStatuses(ctx context.Context, status string, limit int) ([]RunStatus, error)
}
