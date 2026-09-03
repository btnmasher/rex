package notifications

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalidAlertSelector identifies malformed alert selector syntax.
	ErrInvalidAlertSelector = errors.New("invalid alert selector")
	// ErrUnknownAlertSelector identifies a selector that is not in the alert catalog.
	ErrUnknownAlertSelector = errors.New("unknown alert selector")
)

func invalidAlertSelector(selector string) error {
	return fmt.Errorf("%w %q", ErrInvalidAlertSelector, selector)
}

func unknownAlertSelector(selector string) error {
	return fmt.Errorf("%w %q", ErrUnknownAlertSelector, selector)
}
