package token

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNoUsableToken indicates that a corporation has no currently usable credential.
	ErrNoUsableToken = errors.New("no usable EVE token for corporation")
	// ErrNotificationCooldown indicates that the corporation is not due for another normal notification request.
	ErrNotificationCooldown = errors.New("EVE notification token cooldown is active")
)

// NoUsableTokenError explains why a corporation has no selectable access token.
type NoUsableTokenError struct {
	CorporationID string
	Reason        string
	TokenCount    int
	EmptyCount    int
	ExpiredCount  int
}

// Error returns a safe diagnostic message without including token values.
func (e *NoUsableTokenError) Error() string {
	return fmt.Sprintf(
		"%s: corporation %s (%s; tokens=%d empty=%d expired=%d)",
		ErrNoUsableToken,
		e.CorporationID,
		e.Reason,
		e.TokenCount,
		e.EmptyCount,
		e.ExpiredCount,
	)
}

// Unwrap allows callers to identify all unusable-token errors with errors.Is.
func (e *NoUsableTokenError) Unwrap() error { return ErrNoUsableToken }

// CooldownError explains why a corporation's next notification poll is delayed.
type CooldownError struct {
	CorporationID string
	Reason        string
	RetryAfter    time.Duration
}

// Error returns a diagnostic cooldown message while preserving errors.Is compatibility.
func (e *CooldownError) Error() string {
	return fmt.Sprintf("%s: corporation %s (%s; retry after %s)", ErrNotificationCooldown, e.CorporationID, e.Reason, e.RetryAfter)
}

// Unwrap allows callers to identify all cooldown errors with errors.Is.
func (e *CooldownError) Unwrap() error { return ErrNotificationCooldown }
