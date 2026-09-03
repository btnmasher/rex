package esi

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Notification is the subset of an ESI character notification used by rex.
type Notification struct {
	ID         int64     `json:"notification_id"`
	Type       string    `json:"type"`
	SenderID   int64     `json:"sender_id"`
	SenderType string    `json:"sender_type"`
	Timestamp  time.Time `json:"timestamp"`
	// IsRead is decoded for wire compatibility but intentionally has no effect
	// on polling. Rex uses cursor and notification-ID deduplication state.
	IsRead bool   `json:"is_read"`
	Text   string `json:"text"`
}

// Validate checks the fields required to safely identify and order a notification.
func (n *Notification) Validate() error {
	if n == nil {
		return errors.New("notification is nil")
	}
	if n.ID <= 0 {
		return errors.New("notification ID must be positive")
	}
	if strings.TrimSpace(n.Type) == "" {
		return errors.New("notification type is empty")
	}
	if n.Timestamp.IsZero() {
		return errors.New("notification timestamp is missing")
	}
	return nil
}

// Notifications returns the character's ESI notification list.
func (c *Client) Notifications(ctx context.Context, characterID, accessToken string) ([]Notification, error) {
	endpoint, err := characterNotificationsEndpoint(characterID)
	if err != nil {
		return nil, err
	}
	response, err := DoJSON[[]Notification](ctx, c, &Request{
		Endpoint:         endpoint,
		AccessToken:      accessToken,
		RateLimitUserKey: c.rateLimitUserKey(characterID),
	})
	if err != nil {
		return nil, err
	}
	return response.Value, nil
}

func (c *Client) rateLimitUserKey(characterID string) string {
	if c.clientID == "" {
		return ""
	}
	return c.clientID + ":" + strings.TrimSpace(characterID)
}
