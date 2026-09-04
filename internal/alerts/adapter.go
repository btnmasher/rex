package alerts

import (
	"context"
	"errors"

	"github.com/btnmasher/rex/internal/delivery"
	"github.com/btnmasher/rex/internal/enrichment"
)

// DiscordAdapter adapts the alert renderer and Discord transport to the
// provider-neutral delivery worker.
type DiscordAdapter struct {
	service *Service
}

// NewDiscordAdapter creates a delivery adapter backed by an alert service.
func NewDiscordAdapter(service *Service) (*DiscordAdapter, error) {
	if service == nil {
		return nil, errors.New("alert service is required")
	}
	return &DiscordAdapter{service: service}, nil
}

// Deliver renders and sends one enriched alert to a configured Discord target.
func (a *DiscordAdapter) Deliver(ctx context.Context, view *enrichment.Context, targetID string) delivery.Outcome {
	if a == nil || a.service == nil {
		return delivery.Outcome{Status: delivery.OutcomePermanent, Err: errors.New("discord adapter is unavailable")}
	}
	return a.service.DeliverTarget(ctx, view, targetID)
}
