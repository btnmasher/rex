package jobs

import (
	"context"
	"testing"

	"github.com/btnmasher/rex/jobruntime/jobstore"
)

type testCheckpoint struct {
	Offset int64 `json:"offset"`
}

func TestJSONCheckpointCodecRoundTrip(t *testing.T) {
	codec, err := NewJSONCheckpointCodec[testCheckpoint]("test-v1")
	if err != nil {
		t.Fatalf("new checkpoint codec: %v", err)
	}
	want := testCheckpoint{Offset: 42}
	encoded, err := codec.Encode(want)
	if err != nil {
		t.Fatalf("encode checkpoint: %v", err)
	}
	got, err := codec.Decode(encoded)
	if err != nil {
		t.Fatalf("decode checkpoint: %v", err)
	}
	if got != want {
		t.Fatalf("checkpoint = %+v, want %+v", got, want)
	}

	if _, err := codec.Decode(jobstore.Checkpoint{Namespace: "other-v1", Value: []byte(`{"offset":42}`)}); err == nil {
		t.Fatal("expected namespace mismatch")
	}
}

func TestTypedProducerDecodesAndEncodesCheckpoint(t *testing.T) {
	codec, err := NewJSONCheckpointCodec[testCheckpoint]("test-v1")
	if err != nil {
		t.Fatalf("new checkpoint codec: %v", err)
	}
	producer, err := NewTypedProducer(codec, func(
		_ context.Context,
		request TypedProduceRequest[testCheckpoint],
	) (TypedProduceResult[testCheckpoint], error) {
		if request.Checkpoint.Offset != 42 || request.BatchSize != 10 {
			t.Fatalf("unexpected producer request: %+v", request)
		}
		return TypedProduceResult[testCheckpoint]{
			Items:          []jobstore.WorkItem{{ItemID: "43"}},
			NextCheckpoint: testCheckpoint{Offset: 43},
			HasCheckpoint:  true,
			Done:           true,
		}, nil
	})
	if err != nil {
		t.Fatalf("new typed producer: %v", err)
	}
	checkpoint, err := codec.Encode(testCheckpoint{Offset: 42})
	if err != nil {
		t.Fatalf("encode input checkpoint: %v", err)
	}
	result, err := producer.Produce(context.Background(), ProduceRequest{RunID: "run-1", Checkpoint: checkpoint, BatchSize: 10})
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if len(result.Items) != 1 || !result.Done {
		t.Fatalf("unexpected produce result: %+v", result)
	}
	decoded, err := codec.Decode(result.NextCheckpoint)
	if err != nil {
		t.Fatalf("decode output checkpoint: %v", err)
	}
	if decoded.Offset != 43 {
		t.Fatalf("output checkpoint = %+v", decoded)
	}
}

func TestTypedProducerCanOmitCheckpoint(t *testing.T) {
	codec, err := NewJSONCheckpointCodec[testCheckpoint]("test-v1")
	if err != nil {
		t.Fatalf("new checkpoint codec: %v", err)
	}
	producer, err := NewTypedProducer(codec, func(
		context.Context,
		TypedProduceRequest[testCheckpoint],
	) (TypedProduceResult[testCheckpoint], error) {
		return TypedProduceResult[testCheckpoint]{Done: true}, nil
	})
	if err != nil {
		t.Fatalf("new typed producer: %v", err)
	}
	result, err := producer.Produce(context.Background(), ProduceRequest{})
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if !result.NextCheckpoint.Empty() {
		t.Fatalf("expected empty checkpoint, got %+v", result.NextCheckpoint)
	}
}
