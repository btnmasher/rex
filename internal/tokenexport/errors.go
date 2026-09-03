package tokenexport

import (
	"errors"
	"fmt"
)

var (
	// ErrUnauthorized indicates that the token-export bearer credential was rejected.
	ErrUnauthorized = errors.New("token export endpoint unauthorized")
	// ErrInvalidResponse indicates that the token-export response violated its contract.
	ErrInvalidResponse = errors.New("invalid token export response")
)

// HTTPError describes a token-export HTTP failure without retaining response bodies.
type HTTPError struct {
	StatusCode int
	Retryable  bool
}

// Error returns a safe token-export failure description.
func (e *HTTPError) Error() string {
	return fmt.Sprintf("token export endpoint returned status %d", e.StatusCode)
}

// Unwrap identifies authentication failures for callers that need recovery.
func (e *HTTPError) Unwrap() error {
	if e.StatusCode == 401 || e.StatusCode == 403 {
		return ErrUnauthorized
	}
	return nil
}
