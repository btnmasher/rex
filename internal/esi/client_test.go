package esi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewClientRejectsNonHTTPBaseURL(t *testing.T) {
	for _, baseURL := range []string{"ftp://esi.example.test", "%"} {
		t.Run(baseURL, func(t *testing.T) {
			if _, err := NewClient(baseURL, "", nil); err == nil {
				t.Fatalf("NewClient(%q) unexpectedly succeeded", baseURL)
			}
		})
	}
}

func TestNotificationsRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/characters/900000001/notifications" {
			t.Errorf("unexpected path %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("missing bearer token")
		}
		if request.Header.Get("X-Compatibility-Date") != "2026-05-19" {
			t.Errorf("missing compatibility date")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[
			{"notification_id":1,"type":"StructureUnderAttack","sender_id":2,
			"sender_type":"character","timestamp":"2026-08-30T12:00:00Z"}
		]`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "2026-05-19", server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	items, err := client.Notifications(context.Background(), "900000001", "token")
	if err != nil {
		t.Fatalf("notifications: %v", err)
	}
	if len(items) != 1 || items[0].ID != 1 || items[0].Timestamp.IsZero() {
		t.Fatalf("unexpected response: %#v", items)
	}
}

func TestErrorClassifiesRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "0")
		writer.Header().Set("X-Ratelimit-Group", "char-notifications")
		writer.Header().Set("X-Ratelimit-Limit", "150/1m")
		writer.Header().Set("X-Ratelimit-Remaining", "123")
		writer.Header().Set("X-Ratelimit-Used", "27")
		writer.Header().Set("X-Esi-Error-Limit-Remain", "0")
		writer.Header().Set("X-Esi-Error-Limit-Reset", "7")
		writer.Header().Set("X-Request-ID", "request-1")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "", server.Client(), WithRetryConfig(RetryConfig{
		MaxAttempts: 1,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Millisecond,
	}))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = client.Notifications(context.Background(), "900000001", "token")
	var esiErr *Error
	if !errors.As(err, &esiErr) || !esiErr.Retryable() {
		t.Fatalf("expected retryable ESI error, got %v", err)
	}
	if esiErr.Message != "rate limited" || esiErr.RequestID != "request-1" {
		t.Fatalf("unexpected ESI error: %#v", esiErr)
	}
	if !esiErr.RateLimit.Present ||
		esiErr.RateLimit.Group != "char-notifications" ||
		esiErr.RateLimit.Remaining != 123 ||
		esiErr.RateLimit.Used != 27 ||
		esiErr.RateLimit.Reset != 7*time.Second {
		t.Fatalf("unexpected rate-limit metadata: %#v", esiErr.RateLimit)
	}
}

func TestNotificationsUsesConditionalETagRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("If-None-Match") == "" {
			requests.Add(1)
			writer.Header().Set("ETag", `"notification-v1"`)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`[{"notification_id":1}]`))
			return
		}
		if request.Header.Get("If-None-Match") != `"notification-v1"` {
			t.Errorf("unexpected conditional ETag %q", request.Header.Get("If-None-Match"))
		}
		requests.Add(1)
		writer.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "", server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	first, err := client.Notifications(context.Background(), "900000001", "token")
	if err != nil || len(first) != 1 {
		t.Fatalf("first notifications request: items=%#v err=%v", first, err)
	}
	second, err := client.Notifications(context.Background(), "900000001", "token")
	if err != nil || len(second) != 0 {
		t.Fatalf("not-modified notifications request: items=%#v err=%v", second, err)
	}
	if requests.Load() != 2 {
		t.Fatalf("expected two requests, got %d", requests.Load())
	}
}

func TestDoJSONExposesResponseMetadataAndCustomHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/characters/42" {
			t.Errorf("unexpected path %q", request.URL.Path)
		}
		if request.Header.Get("X-Test-Request") != "present" {
			t.Errorf("custom request header was not applied")
		}
		writer.Header().Set("ETag", `"resource-v1"`)
		writer.Header().Set("X-Ratelimit-Group", "char-notifications")
		writer.Header().Set("X-Ratelimit-Limit", "150/1m")
		writer.Header().Set("X-Ratelimit-Remaining", "123")
		writer.Header().Set("X-Ratelimit-Used", "27")
		writer.Header().Set("X-Esi-Error-Limit-Remain", "123")
		writer.Header().Set("X-Esi-Error-Limit-Reset", "4")
		writer.Header().Set("Retry-After", "2")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"name":"Rex"}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL+"/api", "2026-05-19", server.Client(), WithUserAgent("rex-test/1"))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	response, err := DoJSON[struct {
		Name string `json:"name"`
	}](context.Background(), client, &Request{
		Endpoint: Endpoint{Method: http.MethodGet, Path: "/characters/42", CacheKey: "characters/42"},
		Headers:  http.Header{"X-Test-Request": {"present"}},
	})
	if err != nil {
		t.Fatalf("do JSON: %v", err)
	}
	if response.Value.Name != "Rex" || response.Metadata.ETag != `"resource-v1"` {
		t.Fatalf("unexpected response: %#v", response)
	}
	if response.Metadata.RateLimit.Group != "char-notifications" ||
		response.Metadata.RateLimit.Limit != 150 ||
		response.Metadata.RateLimit.Remaining != 123 ||
		response.Metadata.RateLimit.Used != 27 ||
		response.Metadata.RateLimit.Window != time.Minute ||
		response.Metadata.RateLimit.Reset != 4*time.Second {
		t.Fatalf("unexpected rate-limit metadata: %#v", response.Metadata.RateLimit)
	}
	if response.Metadata.RetryAfter != 2*time.Second {
		t.Fatalf("unexpected retry-after: %s", response.Metadata.RetryAfter)
	}
	if response.Metadata.Headers.Get("X-Esi-Error-Limit-Remain") != "123" {
		t.Fatalf("response headers were not retained")
	}
}

func TestNotificationsProvidesRateLimitBucketIdentity(t *testing.T) {
	store := &captureRateLimitStore{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[]`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "", server.Client(), WithClientID("client-1"), WithRateLimitStore(store))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, err := client.Notifications(context.Background(), "900000001", "token"); err != nil {
		t.Fatalf("notifications: %v", err)
	}
	if store.routeKey != "/characters/:id/notifications" || store.userKey != "client-1:900000001" {
		t.Fatalf("unexpected bucket identity: route=%q user=%q", store.routeKey, store.userKey)
	}
}

func TestPublicRequestsProvideClientRateLimitIdentity(t *testing.T) {
	store := &captureRateLimitStore{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"name":"Pure Blind"}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "", server.Client(), WithClientID("client-1"), WithRateLimitStore(store))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, err := client.ResolveRegion(context.Background(), "10000015"); err != nil {
		t.Fatalf("resolve region: %v", err)
	}
	if store.routeKey != "/universe/regions/:id" || store.userKey != "client-1:public" {
		t.Fatalf("unexpected public request identity: route=%q user=%q", store.routeKey, store.userKey)
	}
}

func TestMemoryRateLimitStoreWaitsForExhaustedBucket(t *testing.T) {
	store := NewMemoryRateLimitStore()
	now := time.Now()
	response := &HTTPResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"X-Ratelimit-Group":     {"char-notifications"},
			"X-Ratelimit-Limit":     {"1/1m"},
			"X-Ratelimit-Remaining": {"0"},
			"X-Ratelimit-Used":      {"1"},
		},
	}
	metadata := &ResponseMetadata{}
	store.(*memoryRateLimitStore).Observe("/characters/:id/notifications", "client-1:900000001", response, metadata, now)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := store.Wait(ctx, "/characters/:id/notifications", "client-1:900000001"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected exhausted bucket wait to honor context, got %v", err)
	}
}

func TestMemoryRateLimitStoreReservesMovingWindowCapacity(t *testing.T) {
	store := NewMemoryRateLimitStore()
	now := time.Now().UTC()
	store.(*memoryRateLimitStore).Observe("/characters/:id/notifications", "client-1:900000001", &HTTPResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"X-Ratelimit-Group":     {"char-notifications"},
			"X-Ratelimit-Limit":     {"2/1m"},
			"X-Ratelimit-Remaining": {"2"},
		},
	}, &ResponseMetadata{}, now)
	for range 2 {
		if err := store.Wait(context.Background(), "/characters/:id/notifications", "client-1:900000001"); err != nil {
			t.Fatalf("reserve request: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := store.Wait(ctx, "/characters/:id/notifications", "client-1:900000001"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected moving-window reservation to wait, got %v", err)
	}
}

func TestMemoryRateLimitStoreAccountsForObservedUsage(t *testing.T) {
	store := NewMemoryRateLimitStore().(*memoryRateLimitStore)
	now := time.Now().UTC()
	store.Observe("/characters/:id/notifications", "client-1:900000001", &HTTPResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"X-Ratelimit-Group":     {"char-notifications"},
			"X-Ratelimit-Limit":     {"150/1m"},
			"X-Ratelimit-Remaining": {"123"},
			"X-Ratelimit-Used":      {"27"},
		},
	}, &ResponseMetadata{}, now)
	for range 123 {
		if err := store.Wait(context.Background(), "/characters/:id/notifications", "client-1:900000001"); err != nil {
			t.Fatalf("reserve observed capacity: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := store.Wait(ctx, "/characters/:id/notifications", "client-1:900000001"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected observed usage to reduce available capacity, got %v", err)
	}
}

func TestMemoryCachesEvictExpiredEntries(t *testing.T) {
	etagStore := NewMemoryETagStore().(*memoryETagStore)
	etagStore.items["expired"] = etagEntry{value: "etag", observedAt: time.Now().Add(-etagRetention)}
	if _, ok := etagStore.Get("expired"); ok {
		t.Fatal("expected expired ETag to be evicted")
	}

	rateLimitStore := NewMemoryRateLimitStore().(*memoryRateLimitStore)
	now := time.Now().UTC()
	rateLimitStore.routeGroups["expired-route"] = rateLimitRoute{group: "expired-group", observedAt: now.Add(-rateLimitStateRetention)}
	rateLimitStore.buckets["expired-group\x00user"] = &rateLimitBucket{observedAt: now.Add(-rateLimitStateRetention)}
	rateLimitStore.errorUntil["expired\x00user"] = now.Add(-time.Second)
	rateLimitStore.reserve("unused-route", "user", now)
	if len(rateLimitStore.routeGroups) != 0 || len(rateLimitStore.buckets) != 0 || len(rateLimitStore.errorUntil) != 0 {
		t.Fatalf("stale rate-limit state was retained: routes=%#v buckets=%#v errors=%#v",
			rateLimitStore.routeGroups, rateLimitStore.buckets, rateLimitStore.errorUntil)
	}
}

func TestMiddlewareAllowsNilStores(t *testing.T) {
	wantErr := errors.New("next called")
	next := func(*RequestContext) error { return wantErr }
	request := &RequestContext{}
	if err := ETagMiddleware(nil)(next)(request); !errors.Is(err, wantErr) {
		t.Fatalf("nil ETag store error = %v", err)
	}
	if err := RateLimitMiddleware(nil)(next)(request); !errors.Is(err, wantErr) {
		t.Fatalf("nil rate-limit store error = %v", err)
	}
}

func TestRetryHeadersAreCappedAtThirtyMinutes(t *testing.T) {
	if got, ok := parseSecondsHeader("9999999999"); !ok || got != maxRateLimitCooldown {
		t.Fatalf("parsed oversized retry header = %s, %t", got, ok)
	}
	client, err := NewClient("https://esi.example.test", "", nil, WithRetryConfig(RetryConfig{
		MaxAttempts: 2,
		BaseDelay:   time.Second,
		MaxDelay:    time.Hour,
	}))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if client.retry.MaxDelay != maxRetryDelay {
		t.Fatalf("retry max delay = %s, want %s", client.retry.MaxDelay, maxRetryDelay)
	}
}

type captureRateLimitStore struct {
	routeKey string
	userKey  string
}

func (s *captureRateLimitStore) Wait(_ context.Context, routeKey, userKey string) error {
	s.routeKey = routeKey
	s.userKey = userKey
	return nil
}

func (s *captureRateLimitStore) Observe(routeKey, userKey string, _ *HTTPResponse, _ *ResponseMetadata, _ time.Time) {
	s.routeKey = routeKey
	s.userKey = userKey
}

func TestEndpointValidationRejectsAbsoluteURL(t *testing.T) {
	client, err := NewClient("https://esi.example.test", "", nil)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = DoJSON[struct{}](context.Background(), client, &Request{
		Endpoint: Endpoint{Method: http.MethodGet, Path: "https://evil.example.test/data"},
	})
	if err == nil {
		t.Fatal("expected absolute URL to be rejected")
	}
}
