package notificationstate

import "errors"

var (
	// ErrPendingNotificationPayloadTooLarge identifies a retry payload that
	// exceeds the shared notification-state size limit.
	ErrPendingNotificationPayloadTooLarge = errors.New("pending notification payload exceeds size limit")
)
