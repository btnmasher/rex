package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/btnmasher/rex/jobruntime/jobstore"
)

// Producer supplies work items to a run in bounded batches.
// Producers may use a checkpoint or return no checkpoint for queue-only jobs.
type Producer interface {
	Produce(context.Context, ProduceRequest) (ProduceResult, error)
}

// ProduceRequest describes the producer progress and batch limit for one call.
type ProduceRequest struct {
	RunID      string
	Checkpoint jobstore.Checkpoint
	BatchSize  int
}

// ProduceResult contains newly discovered work and optional producer progress.
type ProduceResult struct {
	Items          []jobstore.WorkItem
	NextCheckpoint jobstore.Checkpoint
	Done           bool
}

// CheckpointCodec converts a domain checkpoint to and from the runtime format.
type CheckpointCodec[C any] interface {
	Encode(C) (jobstore.Checkpoint, error)
	Decode(jobstore.Checkpoint) (C, error)
}

// JSONCheckpointCodec serializes a typed checkpoint as JSON under a namespace.
type JSONCheckpointCodec[C any] struct {
	namespace string
}

// NewJSONCheckpointCodec creates a JSON checkpoint codec for a stable namespace.
func NewJSONCheckpointCodec[C any](namespace string) (*JSONCheckpointCodec[C], error) {
	if strings.TrimSpace(namespace) == "" {
		return nil, errors.New("checkpoint namespace must not be empty")
	}
	return &JSONCheckpointCodec[C]{namespace: namespace}, nil
}

// Encode serializes a typed checkpoint into the runtime checkpoint format.
func (c *JSONCheckpointCodec[C]) Encode(value C) (jobstore.Checkpoint, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return jobstore.Checkpoint{}, fmt.Errorf("encode checkpoint: %w", err)
	}
	return jobstore.Checkpoint{Namespace: c.namespace, Value: encoded}, nil
}

// Decode validates the namespace and deserializes a typed checkpoint.
func (c *JSONCheckpointCodec[C]) Decode(checkpoint jobstore.Checkpoint) (C, error) {
	var value C
	if checkpoint.Empty() {
		return value, nil
	}
	if err := checkpoint.Validate(); err != nil {
		return value, err
	}
	if checkpoint.Namespace != c.namespace {
		return value, fmt.Errorf("checkpoint namespace %q does not match %q", checkpoint.Namespace, c.namespace)
	}
	if err := json.Unmarshal(checkpoint.Value, &value); err != nil {
		return value, fmt.Errorf("decode checkpoint: %w", err)
	}
	return value, nil
}

// TypedProduceRequest presents a decoded checkpoint to a typed producer.
type TypedProduceRequest[C any] struct {
	RunID      string
	Checkpoint C
	BatchSize  int
}

// TypedProduceResult contains typed producer progress and discovered work.
type TypedProduceResult[C any] struct {
	Items          []jobstore.WorkItem
	NextCheckpoint C
	HasCheckpoint  bool
	Done           bool
}

// TypedProducer adapts a typed checkpoint producer to the runtime Producer interface.
type TypedProducer[C any] struct {
	codec   CheckpointCodec[C]
	produce func(context.Context, TypedProduceRequest[C]) (TypedProduceResult[C], error)
}

var _ Producer = (*TypedProducer[struct{}])(nil)

// NewTypedProducer creates a producer with typed checkpoint serialization.
func NewTypedProducer[C any](
	codec CheckpointCodec[C],
	produce func(context.Context, TypedProduceRequest[C]) (TypedProduceResult[C], error),
) (*TypedProducer[C], error) {
	if codec == nil || produce == nil {
		return nil, errors.New("typed producer dependencies are required")
	}
	return &TypedProducer[C]{codec: codec, produce: produce}, nil
}

// Produce decodes the runtime checkpoint, invokes the typed producer, and encodes progress.
func (p *TypedProducer[C]) Produce(ctx context.Context, request ProduceRequest) (ProduceResult, error) {
	checkpoint, err := p.codec.Decode(request.Checkpoint)
	if err != nil {
		return ProduceResult{}, err
	}
	result, err := p.produce(ctx, TypedProduceRequest[C]{RunID: request.RunID, Checkpoint: checkpoint, BatchSize: request.BatchSize})
	if err != nil {
		return ProduceResult{}, err
	}
	output := ProduceResult{Items: result.Items, Done: result.Done}
	if result.HasCheckpoint {
		output.NextCheckpoint, err = p.codec.Encode(result.NextCheckpoint)
		if err != nil {
			return ProduceResult{}, err
		}
	}
	return output, nil
}
