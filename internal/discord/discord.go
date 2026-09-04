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
	"sync"
	"time"
	"unicode/utf8"
)

const (
	defaultWebhookTimeout = 15 * time.Second
	maxErrorBodyBytes     = 4096
	maxPayloadBytes       = 64 << 10
	defaultRetryDelay     = 500 * time.Millisecond
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
	validateURL func(string) error
	logger      *slog.Logger
	gateMu      sync.Mutex
	cooldowns   *webhookCooldowns
}

// RetryableError reports a Discord response that should be retried later.
// The caller owns retry scheduling; Deliver never sleeps for this delay.
type RetryableError struct {
	StatusCode int
	Delay      time.Duration
	Err        error
}

// Error describes the retryable Discord response without exposing response data.
func (e *RetryableError) Error() string {
	if e == nil {
		return "Discord delivery is retryable"
	}
	if e.StatusCode == 0 {
		return "Discord webhook request failed; retryable"
	}
	return fmt.Sprintf("Discord webhook returned retryable status %d", e.StatusCode)
}

// Unwrap returns the safe underlying transport error when one exists.
func (e *RetryableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
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
		validateURL: ValidateWebhookURL,
		logger:      slog.Default(),
		cooldowns:   newWebhookCooldowns(),
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

// Deliver posts one embed message. Rate limits and server errors return a
// RetryableError for the caller's durable retry queue instead of sleeping.
func (d *WebhookDelivery) Deliver(ctx context.Context, destination Destination, message *Message) error {
	if d == nil {
		return errors.New("discord webhook delivery is required")
	}
	if ctx == nil {
		return errors.New("discord delivery context is required")
	}
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
	ensureMentionPolicy(message)
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
	logger := d.deliveryLogger()
	logger.Debug("Discord webhook delivery started",
		"destination_id", destinationID,
		"payload_bytes", len(body),
		"embed_count", len(message.Embeds),
	)
	webhookKey := strings.TrimRight(strings.TrimSpace(destination.WebhookURL), "/")
	if cooldown := d.webhookCooldowns().remaining(webhookKey); cooldown > 0 {
		logger.Warn("Discord webhook delivery deferred by Discord cooldown",
			"destination_id", destinationID,
			"delay", cooldown,
		)
		return &RetryableError{StatusCode: http.StatusTooManyRequests, Delay: cooldown}
	}

	deliveryStartedAt := time.Now()
	attemptStartedAt := time.Now()
	response, err := d.post(ctx, destination.WebhookURL, body)
	if err != nil {
		return d.handleRequestError(logger, destinationID, attemptStartedAt, err)
	}
	responseRetryAfter := retryAfterResponse(response)
	d.logAttempt(response, responseRetryAfter, destinationID, attemptStartedAt)
	if response.statusCode >= http.StatusOK && response.statusCode < http.StatusMultipleChoices {
		d.logDeliveryCompleted(destinationID, deliveryStartedAt)
		return nil
	}
	if response.statusCode == http.StatusTooManyRequests || response.statusCode >= http.StatusInternalServerError {
		delay := retryDelay(response)
		d.webhookCooldowns().set(webhookKey, delay)
		logger.Warn("Discord webhook delivery queued for retry",
			"destination_id", destinationID,
			"delay", delay,
			"status_code", response.statusCode,
		)
		return &RetryableError{StatusCode: response.statusCode, Delay: delay}
	}
	logger.Warn("Discord webhook delivery failed",
		"destination_id", destinationID,
		"attempt", 1,
		"status_code", response.statusCode,
	)
	return fmt.Errorf("discord webhook returned status %d", response.statusCode)
}

func ensureMentionPolicy(message *Message) {
	if message == nil || message.AllowedMentions.Parse != nil {
		return
	}
	message.AllowedMentions.Parse = []string{}
}

func (d *WebhookDelivery) handleRequestError(logger *slog.Logger, destinationID string, startedAt time.Time, err error) error {
	if errors.Is(err, context.Canceled) {
		logger.Warn("Discord webhook attempt canceled",
			"destination_id", destinationID,
			"attempt", 1,
			"duration", time.Since(startedAt),
			"err", err,
		)
		return err
	}
	logger.Warn("Discord webhook attempt failed; retry scheduled",
		"destination_id", destinationID,
		"attempt", 1,
		"duration", time.Since(startedAt),
		"err", err,
	)
	return &RetryableError{Delay: defaultRetryDelay, Err: err}
}

func (d *WebhookDelivery) logAttempt(response webhookResponse, responseRetryAfter time.Duration, destinationID string, attemptStartedAt time.Time) {
	logAttempt := d.deliveryLogger().Debug
	if response.statusCode == http.StatusTooManyRequests || response.statusCode >= http.StatusInternalServerError {
		logAttempt = d.deliveryLogger().Warn
	}
	logAttempt("Discord webhook attempt completed",
		"destination_id", destinationID,
		"attempt", 1,
		"status_code", response.statusCode,
		"response_bytes", len(response.body),
		"duration", time.Since(attemptStartedAt),
		"retry_after", responseRetryAfter,
	)
}

func (d *WebhookDelivery) logDeliveryCompleted(destinationID string, deliveryStartedAt time.Time) {
	d.deliveryLogger().Debug("Discord webhook delivery completed",
		"destination_id", destinationID,
		"attempts", 1,
		"duration", time.Since(deliveryStartedAt),
	)
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

func retryDelay(response webhookResponse) time.Duration {
	delay := retryAfterResponse(response)
	if delay <= 0 {
		delay = defaultRetryDelay
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
	resp, err := d.httpClient().Do(req)
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

func (d *WebhookDelivery) deliveryLogger() *slog.Logger {
	if d != nil && d.logger != nil {
		return d.logger
	}
	return slog.Default()
}

func (d *WebhookDelivery) httpClient() *http.Client {
	if d != nil && d.client != nil {
		return d.client
	}
	return &http.Client{Timeout: defaultWebhookTimeout}
}

func (d *WebhookDelivery) webhookCooldowns() *webhookCooldowns {
	d.gateMu.Lock()
	defer d.gateMu.Unlock()
	if d.cooldowns == nil {
		d.cooldowns = newWebhookCooldowns()
	}
	return d.cooldowns
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
