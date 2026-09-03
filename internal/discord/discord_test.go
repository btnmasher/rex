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

func TestWebhookDeliveryDisablesMentions(t *testing.T) {
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
	if values, ok := mentions["parse"].([]any); !ok || len(values) != 0 {
		t.Fatalf("allowed_mentions.parse = %#v", mentions["parse"])
	}
}

func TestDeliveryRetryDelayIsCapped(t *testing.T) {
	if got := deliveryRetryDelay(1, webhookResponse{headers: http.Header{"Retry-After": []string{"999999999"}}}); got != 30*time.Minute {
		t.Fatalf("retry delay = %s, want 30m", got)
	}
	if got := deliveryRetryDelay(1, webhookResponse{headers: http.Header{"Retry-After": []string{"NaN"}}}); got != webhookRetryDelay+webhookRetryBuffer {
		t.Fatalf("invalid retry delay = %s, want %s", got, webhookRetryDelay+webhookRetryBuffer)
	}
}

func TestWebhookDeliveryRetriesRateLimitAndCompletes(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			writer.Header().Set("Retry-After", "0.001")
			writer.WriteHeader(http.StatusTooManyRequests)
			_, _ = writer.Write([]byte(`{"message":"rate limited","retry_after":0.001}`))
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	delivery := NewWebhookDelivery(server.Client())
	delivery.validateURL = func(string) error { return nil }
	if err := delivery.Deliver(context.Background(), Destination{WebhookURL: server.URL}, &Message{Embeds: []Embed{{Description: "alert"}}}); err != nil {
		t.Fatalf("deliver after rate limit: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("webhook attempts = %d, want 2", attempts)
	}
}

func TestDeliveryRetryDelayUsesJSONRetryAfter(t *testing.T) {
	response := webhookResponse{body: []byte(`{"retry_after":1.25}`)}
	if got := retryAfterResponse(response); got != 1250*time.Millisecond {
		t.Fatalf("JSON retry delay = %s, want 1.25s", got)
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
	delivery.maxTry = 1
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
