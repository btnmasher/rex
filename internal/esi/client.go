// Package esi contains typed clients for the EVE ESI HTTP API.
package esi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	defaultBaseURL       = "https://esi.evetech.net"
	defaultCompatibility = "2026-05-19"
	defaultUserAgent     = "rex-esi-client"
	maxResponseBytes     = 4 << 20
	maxErrorMessageSize  = 1024
	maxAttempts          = 3
	defaultHTTPTimeout   = 20 * time.Second
	retryBaseDelay       = 250 * time.Millisecond
	maxRetryDelay        = 30 * time.Minute
	statusESIRateLimited = 420
)

// RetryConfig controls bounded retries for transient ESI failures.
type RetryConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

func defaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts: maxAttempts,
		BaseDelay:   retryBaseDelay,
		MaxDelay:    maxRetryDelay,
	}
}

// ClientOption customizes a Client without exposing its internal transport state.
type ClientOption func(*clientOptions) error

type clientOptions struct {
	retry          RetryConfig
	userAgent      string
	clientID       string
	logger         *slog.Logger
	etagStore      ETagStore
	rateLimitStore RateLimitStore
	middleware     []Middleware
}

// WithLogger supplies the logger used for redacted ESI request diagnostics.
func WithLogger(logger *slog.Logger) ClientOption {
	return func(options *clientOptions) error {
		if logger == nil {
			return errors.New("ESI logger must not be nil")
		}
		options.logger = logger
		return nil
	}
}

// WithRetryConfig replaces the default bounded retry policy.
func WithRetryConfig(config RetryConfig) ClientOption {
	return func(options *clientOptions) error {
		if config.MaxAttempts <= 0 || config.BaseDelay <= 0 || config.MaxDelay <= 0 {
			return errors.New("ESI retry config values must be positive")
		}
		if config.MaxDelay < config.BaseDelay {
			return errors.New("ESI retry max delay must not be less than base delay")
		}
		if config.MaxDelay > maxRetryDelay {
			config.MaxDelay = maxRetryDelay
		}
		if config.MaxDelay < config.BaseDelay {
			return errors.New("ESI retry max delay exceeds the 30-minute safety cap")
		}
		options.retry = config
		return nil
	}
}

// WithUserAgent sets the User-Agent sent to ESI.
func WithUserAgent(userAgent string) ClientOption {
	return func(options *clientOptions) error {
		userAgent = strings.TrimSpace(userAgent)
		if userAgent == "" {
			return errors.New("ESI user agent must not be empty")
		}
		options.userAgent = userAgent
		return nil
	}
}

// WithETagStore supplies the cache used for conditional requests.
func WithETagStore(store ETagStore) ClientOption {
	return func(options *clientOptions) error {
		if store == nil {
			return errors.New("ESI ETag store must not be nil")
		}
		options.etagStore = store
		return nil
	}
}

// WithClientID configures the ESI client identifier used in rate-limit bucket keys.
func WithClientID(clientID string) ClientOption {
	return func(options *clientOptions) error {
		clientID = strings.TrimSpace(clientID)
		if clientID == "" {
			return errors.New("ESI client ID must not be empty")
		}
		options.clientID = clientID
		return nil
	}
}

// WithRateLimitStore supplies the coordinator used for ESI rate-limit buckets.
func WithRateLimitStore(store RateLimitStore) ClientOption {
	return func(options *clientOptions) error {
		if store == nil {
			return errors.New("ESI rate-limit store must not be nil")
		}
		options.rateLimitStore = store
		return nil
	}
}

// Error describes a structured ESI HTTP failure.
type Error struct {
	StatusCode int
	RetryAfter time.Duration
	RateLimit  RateLimit
	RequestID  string
	Message    string
}

// Error returns a redacted error string suitable for logs.
func (e *Error) Error() string {
	if e == nil {
		return "ESI request failed"
	}
	if e.Message == "" {
		return fmt.Sprintf("ESI request failed with status %d", e.StatusCode)
	}
	return fmt.Sprintf("ESI request failed with status %d: %s", e.StatusCode, e.Message)
}

// Retryable reports whether the request may be retried by the transport.
func (e *Error) Retryable() bool {
	if e == nil {
		return false
	}
	return retryableStatus(e.StatusCode)
}

// Request describes one relative ESI API request.
type Request struct {
	Endpoint         Endpoint
	AccessToken      string
	RateLimitUserKey string
	Headers          http.Header
}

// Response contains a decoded ESI value and transport metadata.
type Response[T any] struct {
	Value    T
	Metadata ResponseMetadata
}

// Client is a reusable ESI transport with endpoint-specific methods.
type Client struct {
	baseURL       *url.URL
	compatibility string
	userAgent     string
	clientID      string
	logger        *slog.Logger
	httpClient    *http.Client
	retry         RetryConfig
	tripper       RoundTripper
}

// NewClient creates an ESI client with bounded transport behavior.
func NewClient(baseURL, compatibility string, httpClient *http.Client, options ...ClientOption) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultBaseURL
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid ESI base URL %q", baseURL)
	}
	if compatibility == "" {
		compatibility = defaultCompatibility
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}

	clientOptions := clientOptions{
		retry:          defaultRetryConfig(),
		userAgent:      defaultUserAgent,
		logger:         slog.Default(),
		etagStore:      NewMemoryETagStore(),
		rateLimitStore: NewMemoryRateLimitStore(),
	}
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(&clientOptions); err != nil {
			return nil, err
		}
	}

	client := &Client{
		baseURL:       parsed,
		compatibility: compatibility,
		userAgent:     clientOptions.userAgent,
		clientID:      clientOptions.clientID,
		logger:        clientOptions.logger,
		httpClient:    httpClient,
		retry:         clientOptions.retry,
	}
	middlewares := []Middleware{
		ETagMiddleware(clientOptions.etagStore),
		RateLimitMiddleware(clientOptions.rateLimitStore),
	}
	middlewares = append(middlewares, clientOptions.middleware...)
	client.tripper = RoundTripper(client.execute)
	for _, middleware := range slices.Backward(middlewares) {
		client.tripper = middleware(client.tripper)
	}
	return client, nil
}

// DoJSON executes an endpoint request and decodes a successful JSON response.
// A 304 response returns a zero Value with Metadata.NotModified set.
func DoJSON[T any](ctx context.Context, c *Client, request *Request) (Response[T], error) {
	if ctx == nil {
		return Response[T]{}, errors.New("ESI request context is required")
	}
	if c == nil {
		return Response[T]{}, errors.New("ESI client must not be nil")
	}
	if request == nil {
		return Response[T]{}, errors.New("ESI request must not be nil")
	}
	if err := request.Endpoint.Validate(); err != nil {
		return Response[T]{}, err
	}

	for attempt := 1; attempt <= c.retry.MaxAttempts; attempt++ {
		response, retry, delay, err := doJSONAttempt[T](ctx, c, request, attempt)
		if err != nil {
			return Response[T]{}, err
		}
		if !retry {
			return response, nil
		}
		delay = c.effectiveRetryDelay(attempt, delay)
		c.logger.Debug("ESI request retry scheduled",
			"path", request.Endpoint.Path,
			"attempt", attempt,
			"max_attempts", c.retry.MaxAttempts,
			"delay", delay,
		)
		if err := c.wait(ctx, attempt, delay); err != nil {
			return Response[T]{}, err
		}
	}

	return Response[T]{}, errors.New("ESI request exhausted retry policy")
}

func doJSONAttempt[T any](ctx context.Context, c *Client, request *Request, attempt int) (Response[T], bool, time.Duration, error) {
	response, err := c.doRequest(ctx, request, attempt)
	if err != nil {
		if ctxErr := contextError(ctx); ctxErr != nil {
			return Response[T]{}, false, 0, ctxErr
		}
		if attempt == c.retry.MaxAttempts {
			return Response[T]{}, false, 0, fmt.Errorf("ESI request: %w", err)
		}
		return Response[T]{}, true, 0, nil
	}

	metadata := response.Metadata
	if metadata.NotModified {
		return Response[T]{Metadata: metadata}, false, 0, nil
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		esiErr := responseError(&response, &metadata)
		if !esiErr.Retryable() || attempt == c.retry.MaxAttempts {
			return Response[T]{}, false, 0, esiErr
		}
		return Response[T]{}, true, retryDelay(&metadata), nil
	}

	var value T
	if err := json.Unmarshal(response.Body, &value); err != nil {
		return Response[T]{}, false, 0, fmt.Errorf("decode ESI response: %w", err)
	}
	return Response[T]{Value: value, Metadata: metadata}, false, 0, nil
}

func (c *Client) doRequest(ctx context.Context, request *Request, attempt int) (rawResponse, error) {
	requestURL, err := c.endpointURL(request.Endpoint.Path)
	if err != nil {
		return rawResponse{}, err
	}

	httpRequest, err := http.NewRequestWithContext(ctx, request.Endpoint.Method, requestURL.String(), http.NoBody)
	if err != nil {
		return rawResponse{}, err
	}
	for key, values := range request.Headers {
		for _, value := range values {
			httpRequest.Header.Add(key, value)
		}
	}
	httpRequest.Header.Set(headerAccept, mediaTypeJSON)
	httpRequest.Header.Set(headerCompatibilityDate, c.compatibility)
	httpRequest.Header.Set(headerUserAgent, c.userAgent)
	if request.AccessToken != "" {
		httpRequest.Header.Set(headerAuthorization, bearerPrefix+request.AccessToken)
	}

	requestContext := &RequestContext{
		Context:     ctx,
		Request:     request,
		Endpoint:    request.Endpoint,
		RouteKey:    request.Endpoint.RouteKey,
		UserKey:     c.rateLimitUserKeyForRequest(request),
		Attempt:     attempt,
		HTTPRequest: httpRequest,
		Metadata:    &ResponseMetadata{},
	}
	if requestContext.RouteKey == "" {
		requestContext.RouteKey = normalizeRouteKey(request.Endpoint.Path)
	}
	startedAt := time.Now()
	requestErr := c.tripper(requestContext)
	c.logRequest(requestContext, time.Since(startedAt), requestErr)
	if requestErr != nil {
		return rawResponse{}, requestErr
	}
	if requestContext.Response == nil {
		return rawResponse{}, errors.New("ESI middleware returned without a response")
	}
	return rawResponse{
		StatusCode: requestContext.Response.StatusCode,
		Headers:    requestContext.Response.Headers,
		Body:       requestContext.Response.Body,
		Metadata:   *requestContext.Metadata,
	}, nil
}

func (c *Client) rateLimitUserKeyForRequest(request *Request) string {
	if request != nil && request.RateLimitUserKey != "" {
		return request.RateLimitUserKey
	}
	if c.clientID == "" {
		return "public"
	}
	return c.clientID + ":public"
}

func (c *Client) logRequest(request *RequestContext, duration time.Duration, requestErr error) {
	attrs := []any{
		"method", request.Endpoint.Method,
		"path", request.Endpoint.Path,
		"route", request.RouteKey,
		"attempt", request.Attempt,
		"duration", duration,
	}
	if request.Response != nil {
		attrs = append(attrs,
			"status_code", request.Response.StatusCode,
			"response_bytes", len(request.Response.Body),
			"request_id", request.Response.Headers.Get(headerRequestID),
		)
	}
	if request.Metadata != nil {
		rateLimit := request.Metadata.RateLimit
		attrs = append(attrs,
			"not_modified", request.Metadata.NotModified,
			"etag_present", request.Metadata.ETag != "",
			"rate_limit_group", rateLimit.Group,
			"rate_limit_limit", rateLimit.Limit,
			"rate_limit_remaining", rateLimit.Remaining,
			"rate_limit_used", rateLimit.Used,
			"rate_limit_window", rateLimit.Window,
			"rate_limit_retry_after", rateLimit.RetryAfter,
			"error_limit_remaining", rateLimit.ErrorLimitRemaining,
			"error_limit_reset", rateLimit.ErrorLimitReset,
		)
	}
	if requestErr != nil {
		attrs = append(attrs, "err", requestErr)
	}
	c.logger.Debug("ESI request", attrs...)
}

func (c *Client) execute(request *RequestContext) error {
	response, err := c.httpClient.Do(request.HTTPRequest)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxResponseBytes {
		return errors.New("ESI response exceeded size limit")
	}

	request.Response = &HTTPResponse{
		StatusCode: response.StatusCode,
		Headers:    response.Header.Clone(),
		Body:       body,
	}
	request.Metadata.StatusCode = response.StatusCode
	request.Metadata.Headers = response.Header.Clone()
	request.Metadata.NotModified = response.StatusCode == http.StatusNotModified
	return nil
}

func (c *Client) endpointURL(path string) (*url.URL, error) {
	parsed, err := url.Parse(path)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Path == "" || !strings.HasPrefix(parsed.Path, "/") {
		return nil, fmt.Errorf("invalid ESI endpoint path %q", path)
	}

	resolved := *c.baseURL
	resolved.Path = strings.TrimRight(c.baseURL.Path, "/") + parsed.Path
	resolved.RawPath = ""
	resolved.RawQuery = parsed.RawQuery
	resolved.Fragment = ""
	return &resolved, nil
}

func (c *Client) wait(ctx context.Context, attempt int, preferred time.Duration) error {
	delay := c.effectiveRetryDelay(attempt, preferred)

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryDelay(metadata *ResponseMetadata) time.Duration {
	if metadata.RetryAfter > 0 {
		return metadata.RetryAfter
	}
	return metadata.RateLimit.Reset
}

func responseError(response *rawResponse, metadata *ResponseMetadata) *Error {
	return &Error{
		StatusCode: metadata.StatusCode,
		RetryAfter: metadata.RetryAfter,
		RateLimit:  metadata.RateLimit,
		RequestID:  response.Headers.Get(headerRequestID),
		Message:    responseMessage(response.Body),
	}
}

func responseMessage(body []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && strings.TrimSpace(payload.Error) != "" {
		return limitErrorMessage(payload.Error)
	}
	return limitErrorMessage(string(body))
}

func limitErrorMessage(message string) string {
	message = strings.TrimSpace(message)
	runes := []rune(message)
	if len(runes) <= maxErrorMessageSize {
		return message
	}
	return string(runes[:maxErrorMessageSize]) + "..."
}

func contextError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ctx.Err()
	}
	return nil
}

func (c *Client) effectiveRetryDelay(attempt int, preferred time.Duration) time.Duration {
	if preferred > 0 {
		return min(preferred, c.retry.MaxDelay)
	}
	delay := c.retry.BaseDelay
	for power := 1; power < attempt; power++ {
		if delay >= c.retry.MaxDelay/2 {
			return c.retry.MaxDelay
		}
		delay *= 2
	}
	return min(delay, c.retry.MaxDelay)
}

func retryableStatus(statusCode int) bool {
	switch {
	case statusCode == http.StatusRequestTimeout:
		return true
	case statusCode == statusESIRateLimited:
		return true
	case statusCode == http.StatusTooManyRequests:
		return true
	case statusCode >= http.StatusInternalServerError && statusCode <= 599:
		return true
	default:
		return false
	}
}

type rawResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	Metadata   ResponseMetadata
}
