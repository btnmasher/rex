package jobs

import "context"

// ForEachItem runs fn for each item and checks ctx before every iteration.
//
// Signature requirement:
// fn must be: func(item T) error
//
// Behavior:
// - returns context error immediately if ctx is canceled
// - returns the first error from fn
// - returns nil when all items complete without error
func ForEachItem[T any](ctx context.Context, items []T, fn func(item T) error) error {
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(item); err != nil {
			return err
		}
	}
	return nil
}

// CollectStepResults runs fn for each item with context checks and collects
// one step result per item.
//
// Signature requirement:
// fn must be: func(item W) (StepResult[W], error)
//
// Use this when per-item mapping itself can fail and should abort the batch.
func CollectStepResults[W RuntimeItem](
	ctx context.Context,
	items []W,
	fn func(item W) (StepResult[W], error),
) ([]StepResult[W], error) {
	results := NewStepResultList(items)
	if err := ForEachItem(ctx, items, func(item W) error {
		res, err := fn(item)
		if err != nil {
			return err
		}
		results = append(results, res)
		return nil
	}); err != nil {
		return nil, err
	}
	return results, nil
}

// IterateStepResultsAbortOnError runs fn(ctx, item) for each item with context
// checks and collects one step result per item.
//
// Signature requirement:
// fn must be: func(ctx context.Context, item W) (StepResult[W], error)
//
// Use this when your mapper needs the context and mapper errors should abort
// the whole batch.
func IterateStepResultsAbortOnError[W RuntimeItem](
	ctx context.Context,
	items []W,
	fn func(ctx context.Context, item W) (StepResult[W], error),
) ([]StepResult[W], error) {
	results := NewStepResultList(items)
	if err := ForEachItem(ctx, items, func(item W) error {
		res, err := fn(ctx, item)
		if err != nil {
			return err
		}
		results = append(results, res)
		return nil
	}); err != nil {
		return nil, err
	}
	return results, nil
}

// IterateStepResultsAbortOnErrorWithProgress behaves like
// IterateStepResultsAbortOnError and records progress with rt.Progress.
//
// Signature requirement:
// fn must be: func(ctx context.Context, item W) (StepResult[W], error)
func IterateStepResultsAbortOnErrorWithProgress[W RuntimeItem](
	ctx context.Context,
	rt StepRuntime,
	items []W,
	fn func(ctx context.Context, item W) (StepResult[W], error),
) ([]StepResult[W], error) {
	results, err := IterateStepResultsAbortOnError(ctx, items, fn)
	if err != nil {
		return nil, err
	}
	if err := rt.Progress(ctx, len(results), len(items)); err != nil {
		return nil, err
	}
	return results, nil
}

// IterateStepResults runs fn(ctx, item) for each item with context checks and
// collects one step result per item.
//
// Signature requirement:
// fn must be: func(ctx context.Context, item W) StepResult[W]
//
// This is the preferred helper for step delegates where per-item outcomes are
// fully expressed as StepResult values.
func IterateStepResults[W RuntimeItem](
	ctx context.Context,
	items []W,
	fn func(ctx context.Context, item W) StepResult[W],
) ([]StepResult[W], error) {
	return IterateStepResultsAbortOnError(ctx, items, func(ctx context.Context, item W) (StepResult[W], error) {
		return fn(ctx, item), nil
	})
}

// IterateStepResultsWithProgress behaves like IterateStepResults and records
// progress with rt.Progress.
//
// Signature requirement:
// fn must be: func(ctx context.Context, item W) StepResult[W]
func IterateStepResultsWithProgress[W RuntimeItem](
	ctx context.Context,
	rt StepRuntime,
	items []W,
	fn func(ctx context.Context, item W) StepResult[W],
) ([]StepResult[W], error) {
	return IterateStepResultsAbortOnErrorWithProgress(ctx, rt, items, func(ctx context.Context, item W) (StepResult[W], error) {
		return fn(ctx, item), nil
	})
}
