package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/btnmasher/rex/jobruntime/jobstore"
)

// ItemID identifies one work item.
type ItemID = jobstore.ItemID

// RuntimeItem is the minimal metadata contract the runner needs per item.
type RuntimeItem interface {
	ID() ItemID
	Attempts() int
	RetryGroup() *string
}

// ItemMeta is an embeddable implementation of RuntimeItem.
type ItemMeta struct {
	ItemID       ItemID
	AttemptCount int
	RetryGroupID *string
}

// ID returns the work-item identifier.
func (m ItemMeta) ID() ItemID { return m.ItemID }

// Attempts returns the number of claims made for the item.
func (m ItemMeta) Attempts() int { return m.AttemptCount }

// RetryGroup returns the optional retry-group identifier.
func (m ItemMeta) RetryGroup() *string { return m.RetryGroupID }

// WorkItem is the default JSON-backed runtime item.
type WorkItem struct {
	ItemMeta

	RunID       string
	PayloadJSON []byte
	ResultJSON  []byte
	Transient   RuntimeItem
}

// BoundWorkItem combines runtime metadata with typed payload and result values.
type BoundWorkItem[P any, R any] struct {
	WorkItem

	Payload P
	Result  R
}

// ItemStatus is the terminal or retry outcome returned by a workflow step.
type ItemStatus string

const (
	// ItemStatusSucceeded marks an item as successfully processed by a step.
	ItemStatusSucceeded ItemStatus = "SUCCEEDED"
	// ItemStatusRetry schedules an item for another attempt.
	ItemStatusRetry ItemStatus = "RETRY"
	// ItemStatusFailed marks an item as permanently failed.
	ItemStatusFailed ItemStatus = "FAILED"
	// ItemStatusCanceled marks an item as canceled by workflow policy.
	ItemStatusCanceled ItemStatus = "CANCELED"
)

// EventLevel controls runtime event severity.
type EventLevel string

const (
	// EventLevelDebug records diagnostic detail.
	EventLevelDebug EventLevel = "DEBUG"
	// EventLevelInfo records normal lifecycle information.
	EventLevelInfo EventLevel = "INFO"
	// EventLevelWarn records a recoverable problem.
	EventLevelWarn EventLevel = "WARN"
	// EventLevelError records a failed operation.
	EventLevelError EventLevel = "ERROR"
)

// StepRuntime exposes workflow metadata, diagnostics, and progress reporting.
type StepRuntime interface {
	RunID() string
	JobID() string
	JobKind() string
	StepName() string
	RetryGroupID() *string
	Logger() *slog.Logger
	Debug(ctx context.Context, kind string, attrs ...slog.Attr) error
	Debugf(ctx context.Context, kind, format string, args ...any) error
	Info(ctx context.Context, kind string, attrs ...slog.Attr) error
	Infof(ctx context.Context, kind, format string, args ...any) error
	Warn(ctx context.Context, kind string, attrs ...slog.Attr) error
	Warnf(ctx context.Context, kind, format string, args ...any) error
	Error(ctx context.Context, kind string, attrs ...slog.Attr) error
	Errorf(ctx context.Context, kind, format string, args ...any) error
	Progress(ctx context.Context, done, total int, attrs ...slog.Attr) error
}

// StepResult reports one item's outcome from a workflow step.
type StepResult[W RuntimeItem] struct {
	Item          W
	Status        ItemStatus
	ResultJSON    []byte
	Error         string
	RetryAt       *time.Time
	RetryOverride bool
}

// NewStepResult builds a step result with explicit status/error text.
func NewStepResult[W RuntimeItem](item W, status ItemStatus, errMsg string) StepResult[W] {
	return StepResult[W]{
		Item:   item,
		Status: status,
		Error:  errMsg,
	}
}

// Succeeded creates a successful step result.
func Succeeded[W RuntimeItem](item W) StepResult[W] {
	return NewStepResult(item, ItemStatusSucceeded, "")
}

// Retry creates a retryable step result from an optional error.
func Retry[W RuntimeItem](item W, err error) StepResult[W] {
	if err == nil {
		return NewStepResult(item, ItemStatusRetry, "")
	}

	return NewStepResult(item, ItemStatusRetry, err.Error())
}

// Failed creates a terminal failure step result from an optional error.
func Failed[W RuntimeItem](item W, err error) StepResult[W] {
	if err == nil {
		return NewStepResult(item, ItemStatusFailed, "")
	}

	return NewStepResult(item, ItemStatusFailed, err.Error())
}

// NewStepResultList allocates a result list sized for the supplied items.
func NewStepResultList[W RuntimeItem](items []W) []StepResult[W] {
	return make([]StepResult[W], 0, len(items))
}

// StepDefinition describes one ordered workflow step.
type StepDefinition[W RuntimeItem] struct {
	Name                string
	Critical            bool
	AllowPartialFailure bool
	Delegate            func(ctx context.Context, rt StepRuntime, items []W) ([]StepResult[W], error)
}

// WorkItemCodec converts store records to and from domain work items.
type WorkItemCodec[W RuntimeItem] interface {
	DecodeItem(meta ItemMeta, payloadJSON, resultJSON []byte) (W, error)
	EncodeResult(item W) ([]byte, error)
}

// DefaultWorkItemCodec passes through the untyped WorkItem representation.
type DefaultWorkItemCodec struct{}

func (DefaultWorkItemCodec) DecodeItem(meta ItemMeta, payloadJSON, resultJSON []byte) (WorkItem, error) {
	return WorkItem{
		ItemMeta:    meta,
		PayloadJSON: payloadJSON,
		ResultJSON:  resultJSON,
	}, nil
}

//nolint:gocritic // WorkItemCodec requires value-typed generic parameter.
func (DefaultWorkItemCodec) EncodeResult(item WorkItem) ([]byte, error) {
	return item.ResultJSON, nil
}

// NewWorkItem marshals a typed payload into a new work item instance.
func NewWorkItem[P any, R any](itemID ItemID, payload P) (BoundWorkItem[P, R], error) {
	payloadJSON, err := marshalPayload(payload)
	if err != nil {
		return BoundWorkItem[P, R]{}, err
	}

	return BoundWorkItem[P, R]{
		ItemID:      itemID,
		PayloadJSON: payloadJSON,
		Payload:     payload,
	}, nil
}

// BindWorkItem decodes payload and result JSON into typed values.
func BindWorkItem[P any, R any](item *WorkItem) (BoundWorkItem[P, R], error) {
	if item == nil {
		return BoundWorkItem[P, R]{}, errors.New("work item is required")
	}
	payload, err := unmarshalPayload[P](item.PayloadJSON)
	if err != nil {
		return BoundWorkItem[P, R]{}, err
	}

	result, err := unmarshalPayload[R](item.ResultJSON)
	if err != nil {
		return BoundWorkItem[P, R]{}, err
	}

	return BoundWorkItem[P, R]{
		WorkItem: *item,
		Payload:  payload,
		Result:   result,
	}, nil
}

// DecodePayload decodes a work item's payload into the requested type.
func DecodePayload[P any](item *WorkItem) (P, error) {
	return unmarshalPayload[P](item.PayloadJSON)
}

// DecodeResult decodes a work item's result into the requested type.
func DecodeResult[R any](item *WorkItem) (R, error) {
	return unmarshalPayload[R](item.ResultJSON)
}

func marshalPayload[T any](v T) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	return raw, nil
}

func unmarshalPayload[T any](raw []byte) (T, error) {
	var out T
	if len(raw) == 0 {
		return out, nil
	}

	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("unmarshal payload: %w", err)
	}

	return out, nil
}
