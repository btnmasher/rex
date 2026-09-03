package jobs

import (
	"fmt"
	"runtime/debug"
)

// PipelineError identifies the runtime stage that produced an error.
type PipelineError struct {
	Stage string
	Err   error
}

// Error returns the stage-qualified error message.
func (e *PipelineError) Error() string {
	return fmt.Sprintf("pipeline stage %s: %v", e.Stage, e.Err)
}

// Unwrap exposes the underlying stage error for errors.Is and errors.As.
func (e *PipelineError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type panicError struct {
	operation string
	valueType string
}

func newPanicError(operation string, value any) *panicError {
	return &panicError{
		operation: operation,
		valueType: fmt.Sprintf("%T", value),
	}
}

func (e *panicError) Error() string {
	return fmt.Sprintf("job runtime recovered panic in %s (%s)", e.operation, e.valueType)
}

func capturePanic(fn func()) (panicked bool, value any, stack []byte) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicked = true
			value = recovered
			stack = debug.Stack()
		}
	}()

	fn()
	return false, nil, nil
}
