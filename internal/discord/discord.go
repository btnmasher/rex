// Package discord defines delivery ports and Discord webhook delivery.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultWebhookTimeout = 15 * time.Second
	webhookMaxAttempts    = 3
	maxErrorBodyBytes     = 4096
	maxPayloadBytes       = 64 << 10
	webhookRetryDelay     = 500 * time.Millisecond
	webhookRetryBuffer    = 100 * time.Millisecond
	maxWebhookRetryDelay  = 30 * time.Minute
	maxContentCharacters  = 2000
	maxEmbedCount         = 10
	maxEmbedTitle         = 256
	maxEmbedDescription   = 4096
	maxFieldName          = 256
	maxFieldValue         = 1024
	maxEmbedCharacters    = 6000
	maxUsernameCharacters = 80
	maxAuthorName         = 256
)

const (
	// MaxEmbedFields is the maximum number of fields Discord accepts in one embed.
	MaxEmbedFields = 25
)

// Destination identifies a delivery avenue without coupling callers to webhooks.
type Destination struct {
	ID         string
	WebhookURL string
}

// Field is one named Discord embed field.
type Field struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// Embed is the subset of Discord embeds used by alert messages.
type Embed struct {
	Title       string    `json:"title,omitempty"`
	Description string    `json:"description,omitempty"`
	Color       int       `json:"color,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
	Fields      []Field   `json:"fields,omitempty"`
	Thumbnail   *Image    `json:"thumbnail,omitempty"`
	Author      *Author   `json:"author,omitempty"`
}

// Author is the optional embed author line and icon.
type Author struct {
	Name    string `json:"name"`
	IconURL string `json:"icon_url,omitempty"`
}

// Image is an embed image reference.
type Image struct {
	URL string `json:"url"`
}

// AllowedMentions disables accidental mentions in untrusted notification text.
type AllowedMentions struct {
	Parse []string `json:"parse"`
}

// Message is the provider-neutral Discord message model.
type Message struct {
	Content         string          `json:"content,omitempty"`
	Username        string          `json:"username,omitempty"`
	AvatarURL       string          `json:"avatar_url,omitempty"`
	Embeds          []Embed         `json:"embeds,omitempty"`
	AllowedMentions AllowedMentions `json:"allowed_mentions"`
}

// Delivery sends a message to a configured destination.
type Delivery interface {
	Deliver(context.Context, Destination, *Message) error
}

// WebhookDelivery sends messages through Discord webhooks.
type WebhookDelivery struct {
	client      *http.Client
	maxTry      int
	validateURL func(string) error
	logger      *slog.Logger
}

// WebhookOption customizes webhook delivery diagnostics.
type WebhookOption func(*WebhookDelivery)

// WithLogger supplies the logger used for redacted Discord delivery diagnostics.
func WithLogger(logger *slog.Logger) WebhookOption {
	return func(delivery *WebhookDelivery) {
		if logger != nil {
			delivery.logger = logger
		}
	}
}

type webhookResponse struct {
	statusCode int
	headers    http.Header
	body       []byte
}

type webhookRequestError struct {
	err error
}

func (e *webhookRequestError) Error() string {
	return fmt.Sprintf("Discord webhook request failed (%T)", e.err)
}

func (e *webhookRequestError) Unwrap() error {
	return e.err
}

// NewWebhookDelivery creates a webhook adapter with bounded request behavior.
func NewWebhookDelivery(client *http.Client, options ...WebhookOption) *WebhookDelivery {
	if client == nil {
		client = &http.Client{Timeout: defaultWebhookTimeout}
	}
	safeClient := *client
	safeClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	delivery := &WebhookDelivery{
		client:      &safeClient,
		maxTry:      webhookMaxAttempts,
		validateURL: ValidateWebhookURL,
		logger:      slog.Default(),
	}
	for _, option := range options {
		if option != nil {
			option(delivery)
		}
	}
	return delivery
}

// Validate checks the Discord message limits used by this adapter.
func (m *Message) Validate() error {
	if m == nil {
		return errors.New("discord message is required")
	}
	if err := validateMessageLimits(m); err != nil {
		return err
	}
	totalCharacters, err := validateEmbeds(m.Embeds)
	if err != nil {
		return err
	}
	if totalCharacters > maxEmbedCharacters {
		return errors.New("discord embeds exceed 6000 characters in total")
	}
	return nil
}

func validateMessageLimits(message *Message) error {
	if utf8.RuneCountInString(message.Content) > maxContentCharacters {
		return errors.New("discord message content exceeds 2000 characters")
	}
	if utf8.RuneCountInString(message.Username) > maxUsernameCharacters {
		return errors.New("discord webhook username exceeds 80 characters")
	}
	if len(message.Embeds) > maxEmbedCount {
		return errors.New("discord message contains too many embeds")
	}
	if message.Content == "" && len(message.Embeds) == 0 {
		return errors.New("discord message must contain content or an embed")
	}
	return nil
}

func validateEmbeds(embeds []Embed) (int, error) {
	totalCharacters := 0
	for i := range embeds {
		embedCharacters, err := validateEmbed(&embeds[i])
		if err != nil {
			return 0, err
		}
		totalCharacters += embedCharacters
	}
	return totalCharacters, nil
}

func validateEmbed(embed *Embed) (int, error) {
	if utf8.RuneCountInString(embed.Title) > maxEmbedTitle {
		return 0, errors.New("discord embed title exceeds 256 characters")
	}
	if utf8.RuneCountInString(embed.Description) > maxEmbedDescription {
		return 0, errors.New("discord embed description exceeds 4096 characters")
	}
	if embed.Author != nil && utf8.RuneCountInString(embed.Author.Name) > maxAuthorName {
		return 0, errors.New("discord embed author name exceeds 256 characters")
	}
	if len(embed.Fields) > MaxEmbedFields {
		return 0, errors.New("discord embed contains too many fields")
	}
	embedCharacters := utf8.RuneCountInString(embed.Title) + utf8.RuneCountInString(embed.Description)
	if embed.Author != nil {
		embedCharacters += utf8.RuneCountInString(embed.Author.Name)
	}
	for _, field := range embed.Fields {
		if err := validateField(field); err != nil {
			return 0, err
		}
		embedCharacters += utf8.RuneCountInString(field.Name) + utf8.RuneCountInString(field.Value)
	}
	if embedCharacters > maxEmbedCharacters {
		return 0, errors.New("discord embed exceeds 6000 characters")
	}
	return embedCharacters, nil
}

func validateField(field Field) error {
	if utf8.RuneCountInString(field.Name) == 0 || utf8.RuneCountInString(field.Name) > maxFieldName {
		return errors.New("discord embed field name is empty or exceeds 256 characters")
	}
	if utf8.RuneCountInString(field.Value) == 0 || utf8.RuneCountInString(field.Value) > maxFieldValue {
		return errors.New("discord embed field value is empty or exceeds 1024 characters")
	}
	return nil
}

// Deliver posts one embed message and retries only rate limits and server errors.
func (d *WebhookDelivery) Deliver(ctx context.Context, destination Destination, message *Message) error {
	validateURL := d.validateURL
	if validateURL == nil {
		validateURL = ValidateWebhookURL
	}
	if err := validateURL(destination.WebhookURL); err != nil {
		return err
	}
	if err := message.Validate(); err != nil {
		return err
	}
	message.AllowedMentions = AllowedMentions{Parse: []string{}}
	body, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode Discord message: %w", err)
	}
	if len(body) > maxPayloadBytes {
		return errors.New("discord message exceeds payload limit")
	}
	destinationID := destination.ID
	if destinationID == "" {
		destinationID = "unnamed"
	}
	d.logger.Debug("Discord webhook delivery started",
		"destination_id", destinationID,
		"payload_bytes", len(body),
		"embed_count", len(message.Embeds),
	)
	return d.deliverAttempts(ctx, destination, body, destinationID)
}

func (d *WebhookDelivery) deliverAttempts(ctx context.Context, destination Destination, body []byte, destinationID string) error {
	deliveryStartedAt := time.Now()
	for attempt := 1; attempt <= d.maxTry; attempt++ {
		shouldRetry, err := d.deliverAttempt(ctx, destination, body, destinationID, attempt, deliveryStartedAt)
		if err != nil {
			return err
		}
		if !shouldRetry {
			return nil
		}
	}
	d.logger.Warn("Discord webhook delivery exhausted retries",
		"destination_id", destinationID,
		"attempts", d.maxTry,
	)
	return errors.New("discord delivery exhausted retries")
}

func (d *WebhookDelivery) deliverAttempt(
	ctx context.Context,
	destination Destination,
	body []byte,
	destinationID string,
	attempt int,
	deliveryStartedAt time.Time,
) (bool, error) {
	attemptStartedAt := time.Now()
	response, err := d.post(ctx, destination.WebhookURL, body)
	if err != nil {
		d.logger.Debug("Discord webhook attempt failed",
			"destination_id", destinationID,
			"attempt", attempt,
			"duration", time.Since(attemptStartedAt),
			"err", err,
		)
		return false, err
	}
	responseRetryAfter := retryAfterResponse(response)
	d.logAttempt(response, responseRetryAfter, destinationID, attempt, attemptStartedAt)
	if response.statusCode >= http.StatusOK && response.statusCode < http.StatusMultipleChoices {
		d.logDeliveryCompleted(destinationID, attempt, deliveryStartedAt)
		return false, nil
	}
	if !d.retryable(response.statusCode, attempt) {
		if response.statusCode == http.StatusTooManyRequests || response.statusCode >= http.StatusInternalServerError {
			d.logger.Warn("Discord webhook delivery exhausted retries",
				"destination_id", destinationID,
				"attempts", attempt,
				"status_code", response.statusCode,
			)
			return false, errors.New("discord delivery exhausted retries")
		}
		d.logger.Warn("Discord webhook delivery failed",
			"destination_id", destinationID,
			"attempt", attempt,
			"status_code", response.statusCode,
		)
		return false, fmt.Errorf("discord webhook returned status %d", response.statusCode)
	}
	delay := deliveryRetryDelay(attempt, response)
	d.logger.Warn("Discord webhook retry scheduled",
		"destination_id", destinationID,
		"attempt", attempt,
		"next_attempt", attempt+1,
		"delay", delay,
		"retry_after", responseRetryAfter,
	)
	if err := wait(ctx, delay); err != nil {
		d.logger.Warn("Discord webhook retry interrupted",
			"destination_id", destinationID,
			"attempt", attempt,
			"err", err,
		)
		return false, err
	}
	d.logger.Debug("Discord webhook retry attempt starting",
		"destination_id", destinationID,
		"attempt", attempt+1,
	)
	return true, nil
}

func (d *WebhookDelivery) logAttempt(response webhookResponse, responseRetryAfter time.Duration, destinationID string, attempt int, attemptStartedAt time.Time) {
	logAttempt := d.logger.Debug
	if response.statusCode == http.StatusTooManyRequests || response.statusCode >= http.StatusInternalServerError {
		logAttempt = d.logger.Warn
	}
	logAttempt("Discord webhook attempt completed",
		"destination_id", destinationID,
		"attempt", attempt,
		"status_code", response.statusCode,
		"response_bytes", len(response.body),
		"duration", time.Since(attemptStartedAt),
		"retry_after", responseRetryAfter,
	)
}

func (d *WebhookDelivery) logDeliveryCompleted(destinationID string, attempt int, deliveryStartedAt time.Time) {
	d.logger.Debug("Discord webhook delivery completed",
		"destination_id", destinationID,
		"attempts", attempt,
		"duration", time.Since(deliveryStartedAt),
	)
	if attempt > 1 {
		d.logger.Info("Discord webhook delivery recovered after retry",
			"destination_id", destinationID,
			"attempt", attempt,
			"duration", time.Since(deliveryStartedAt),
		)
	}
}

// ValidateWebhookURL checks that raw is a trusted Discord webhook URL.
func ValidateWebhookURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("invalid Discord webhook URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !strings.HasPrefix(parsed.Path, "/api/webhooks/") {
		return errors.New("invalid Discord webhook URL")
	}
	if parsed.Scheme != "https" {
		return errors.New("discord webhook URL must use HTTPS")
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "discord.com", "discordapp.com", "canary.discord.com", "ptb.discord.com":
		if validWebhookPath(parsed.Path) {
			return nil
		}
		return errors.New("invalid Discord webhook URL path")
	default:
		return errors.New("discord webhook URL host is not trusted")
	}
}

func validWebhookPath(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 4 && parts[0] == "api" && parts[1] == "webhooks" && parts[2] != "" && parts[3] != ""
}

func (d *WebhookDelivery) retryable(statusCode, attempt int) bool {
	return (statusCode == http.StatusTooManyRequests || statusCode >= 500) && attempt < d.maxTry
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func deliveryRetryDelay(attempt int, response webhookResponse) time.Duration {
	delay := retryAfterResponse(response)
	if delay <= 0 {
		delay = time.Duration(attempt) * webhookRetryDelay
	}
	delay = min(delay, maxWebhookRetryDelay)
	if delay < maxWebhookRetryDelay {
		delay += webhookRetryBuffer
	}
	return min(delay, maxWebhookRetryDelay)
}

func retryAfterResponse(response webhookResponse) time.Duration {
	if delay := retryAfter(response.headers.Get("Retry-After")); delay > 0 {
		return delay
	}
	return retryAfterJSON(response.body)
}

func retryAfterJSON(body []byte) time.Duration {
	var payload struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0
	}
	return retryAfter(strconv.FormatFloat(payload.RetryAfter, 'f', -1, 64))
}

func (d *WebhookDelivery) post(ctx context.Context, webhookURL string, body []byte) (webhookResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return webhookResponse{}, &webhookRequestError{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return webhookResponse{}, &webhookRequestError{err: err}
	}
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return webhookResponse{}, &webhookRequestError{err: readErr}
	}
	if closeErr != nil {
		return webhookResponse{}, &webhookRequestError{err: closeErr}
	}
	return webhookResponse{statusCode: resp.StatusCode, headers: resp.Header, body: responseBody}, nil
}

func retryAfter(value string) time.Duration {
	seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0
	}
	if seconds >= maxWebhookRetryDelay.Seconds() {
		return maxWebhookRetryDelay
	}
	return time.Duration(seconds * float64(time.Second))
}
