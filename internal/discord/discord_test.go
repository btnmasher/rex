package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type failingTransport struct {
	err error
}

func (t failingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("transport failed for %s: %w", request.URL, t.err)
}

func TestWebhookDeliveryPreservesMentionPolicy(t *testing.T) {
	requests := make(chan map[string]any, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", request.Method)
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		requests <- payload
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	delivery := NewWebhookDelivery(server.Client())
	delivery.validateURL = func(string) error { return nil }
	err := delivery.Deliver(context.Background(), Destination{WebhookURL: server.URL + "/api/webhooks/123/token"}, &Message{
		Content:         "untrusted @everyone text",
		AllowedMentions: AllowedMentions{Parse: []string{"users"}},
	})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	payload := <-requests
	mentions, ok := payload["allowed_mentions"].(map[string]any)
	if !ok {
		t.Fatalf("allowed_mentions = %#v", payload["allowed_mentions"])
	}
	if values, ok := mentions["parse"].([]any); !ok || len(values) != 1 || values[0] != "users" {
		t.Fatalf("allowed_mentions.parse = %#v", mentions["parse"])
	}
}

func TestWebhookDeliveryDefaultsToNoMentions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload Message
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		if payload.AllowedMentions.Parse == nil || len(payload.AllowedMentions.Parse) != 0 {
			t.Errorf("allowed mentions = %#v", payload.AllowedMentions.Parse)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	delivery := NewWebhookDelivery(server.Client())
	delivery.validateURL = func(string) error { return nil }
	if err := delivery.Deliver(context.Background(), Destination{WebhookURL: server.URL}, &Message{Content: "safe"}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
}

func TestDeliveryRetryDelayIsCapped(t *testing.T) {
	if got := retryDelay(webhookResponse{headers: http.Header{"Retry-After": []string{"999999999"}}}); got != 30*time.Minute {
		t.Fatalf("retry delay = %s, want 30m", got)
	}
	if got := retryDelay(webhookResponse{headers: http.Header{"Retry-After": []string{"NaN"}}}); got != defaultRetryDelay {
		t.Fatalf("invalid retry delay = %s, want %s", got, defaultRetryDelay)
	}
}

func TestWebhookDeliveryReturnsRateLimitToDurableRetryQueue(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			writer.Header().Set("Retry-After", "1")
			writer.WriteHeader(http.StatusTooManyRequests)
			_, _ = writer.Write([]byte(`{"message":"rate limited","retry_after":1}`))
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	delivery := NewWebhookDelivery(server.Client())
	delivery.validateURL = func(string) error { return nil }
	err := delivery.Deliver(context.Background(), Destination{WebhookURL: server.URL}, &Message{Embeds: []Embed{{Description: "alert"}}})
	retryErr, ok := errors.AsType[*RetryableError](err)
	if !ok {
		t.Fatalf("delivery error = %v, want RetryableError", err)
	}
	if retryErr.Delay != time.Second {
		t.Fatalf("retry delay = %s, want 1s", retryErr.Delay)
	}
	if attempts != 1 {
		t.Fatalf("webhook attempts = %d, want 1", attempts)
	}
	cooldownErr := delivery.Deliver(context.Background(), Destination{WebhookURL: server.URL}, &Message{Embeds: []Embed{{Description: "alert"}}})
	if _, ok := errors.AsType[*RetryableError](cooldownErr); !ok {
		t.Fatalf("cooldown error = %v, want RetryableError", cooldownErr)
	}
	if attempts != 1 {
		t.Fatalf("webhook attempts during cooldown = %d, want 1", attempts)
	}
}

func TestWebhookDeliveryNilAndZeroValueAreSafe(t *testing.T) {
	var nilDelivery *WebhookDelivery
	if err := nilDelivery.Deliver(context.Background(), Destination{}, &Message{Embeds: []Embed{{Description: "alert"}}}); err == nil {
		t.Fatal("nil delivery unexpectedly succeeded")
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	zeroDelivery := &WebhookDelivery{validateURL: func(string) error { return nil }}
	if err := zeroDelivery.Deliver(context.Background(), Destination{WebhookURL: server.URL}, &Message{Embeds: []Embed{{Description: "alert"}}}); err != nil {
		t.Fatalf("zero-value delivery: %v", err)
	}
}

func TestDeliveryRetryDelayUsesJSONRetryAfter(t *testing.T) {
	response := webhookResponse{body: []byte(`{"retry_after":1.25}`)}
	if got := retryAfterResponse(response); got != 1250*time.Millisecond {
		t.Fatalf("JSON retry delay = %s, want 1.25s", got)
	}
}

func TestWebhookCooldownsTrackRemoteBackoffWithoutBlocking(t *testing.T) {
	cooldowns := newWebhookCooldowns()
	if got := cooldowns.remaining("webhook"); got != 0 {
		t.Fatalf("initial cooldown = %s, want zero", got)
	}

	cooldowns.set("webhook", time.Second)
	if got := cooldowns.remaining("webhook"); got <= 0 {
		t.Fatalf("tracked cooldown = %s, want positive duration", got)
	}
	if got := cooldowns.remaining("other-webhook"); got != 0 {
		t.Fatalf("unrelated cooldown = %s, want zero", got)
	}
}

func TestWebhookDeliveryRejectsUnsupportedURL(t *testing.T) {
	err := NewWebhookDelivery(nil).Deliver(
		context.Background(),
		Destination{WebhookURL: "ftp://example.invalid/api/webhooks/123/token"},
		&Message{Embeds: []Embed{{Description: "test"}}},
	)
	if err == nil {
		t.Fatal("expected unsupported URL error")
	}
}

func TestWebhookDeliveryRedactsWebhookURLFromTransportErrors(t *testing.T) {
	sentinel := errors.New("transport unavailable")
	secret := "webhook-secret-token"
	webhookURL := "https://discord.com/api/webhooks/123/" + secret
	delivery := NewWebhookDelivery(&http.Client{Transport: failingTransport{err: sentinel}})

	err := delivery.Deliver(context.Background(), Destination{WebhookURL: webhookURL}, &Message{
		Embeds: []Embed{{Description: "alert"}},
	})
	if err == nil {
		t.Fatal("expected transport error")
	}
	if _, ok := errors.AsType[*RetryableError](err); !ok {
		t.Fatalf("error = %v, want RetryableError", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want wrapped transport error", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked webhook secret: %v", err)
	}
}

func TestWebhookDeliveryDoesNotPersistResponseBodyInErrors(t *testing.T) {
	secret := "webhook-secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(secret))
	}))
	defer server.Close()

	delivery := NewWebhookDelivery(server.Client())
	delivery.validateURL = func(string) error { return nil }
	err := delivery.Deliver(context.Background(), Destination{WebhookURL: server.URL + "/api/webhooks/123/" + secret}, &Message{
		Embeds: []Embed{{Description: "alert"}},
	})
	if err == nil {
		t.Fatal("expected HTTP error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked response secret: %v", err)
	}
}

func TestMessageValidationRejectsTooManyFields(t *testing.T) {
	fields := make([]Field, 26)
	for i := range fields {
		fields[i] = Field{Name: "name", Value: "value"}
	}
	err := NewWebhookDelivery(nil).Deliver(
		context.Background(),
		Destination{WebhookURL: "https://discord.com/api/webhooks/123/token"},
		&Message{Embeds: []Embed{{Description: "alert", Fields: fields}}},
	)
	if err == nil {
		t.Fatal("expected Discord field limit error")
	}
}

func TestMessageValidationCountsAuthorInAggregateLimit(t *testing.T) {
	message := &Message{Embeds: []Embed{{
		Title:       strings.Repeat("t", maxEmbedTitle),
		Description: strings.Repeat("d", maxEmbedDescription),
		Author:      &Author{Name: strings.Repeat("a", maxAuthorName)},
		Fields: []Field{
			{Name: strings.Repeat("n", maxFieldName), Value: strings.Repeat("v", maxFieldValue)},
			{Name: "n", Value: strings.Repeat("v", 113)},
		},
	}}}
	if err := message.Validate(); err == nil {
		t.Fatal("expected aggregate embed character limit error")
	}
}
