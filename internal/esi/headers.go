package esi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	headerAccept             = "Accept"
	headerAuthorization      = "Authorization"
	headerCompatibilityDate  = "X-Compatibility-Date"
	headerETag               = "ETag"
	headerIfNoneMatch        = "If-None-Match"
	headerRequestID          = "X-Request-ID"
	headerRetryAfter         = "Retry-After"
	headerUserAgent          = "User-Agent"
	headerRateLimitGroup     = "X-Ratelimit-Group"
	headerRateLimitLimit     = "X-Ratelimit-Limit"
	headerRateLimitRemaining = "X-Ratelimit-Remaining"
	headerRateLimitUsed      = "X-Ratelimit-Used"
	headerErrorLimitRemain   = "X-ESI-Error-Limit-Remain"
	headerErrorLimitReset    = "X-ESI-Error-Limit-Reset"
	mediaTypeJSON            = "application/json"
	bearerPrefix             = "Bearer "

	defaultRateLimitCooldown = 30 * time.Second
	maxRateLimitCooldown     = 30 * time.Minute
	rateLimitStateRetention  = 30 * time.Minute
	rateLimitPruneInterval   = 5 * time.Minute
	etagRetention            = 24 * time.Hour
	maxETagEntries           = 4096
	maxRateLimitRoutes       = 256
	maxRateLimitBuckets      = 4096
	maxRateLimitErrors       = 4096
)

// ETagStore stores validators by endpoint cache key.
type ETagStore interface {
	Get(key string) (string, bool)
	Put(key, etag string)
}

type memoryETagStore struct {
	mu    sync.RWMutex
	items map[string]etagEntry
}

type etagEntry struct {
	value      string
	observedAt time.Time
}

// NewMemoryETagStore creates a concurrency-safe process-local ETag store.
func NewMemoryETagStore() ETagStore {
	return &memoryETagStore{items: make(map[string]etagEntry)}
}

func (s *memoryETagStore) Get(key string) (string, bool) {
	if strings.TrimSpace(key) == "" {
		return "", false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.items[key]
	if !ok {
		return "", false
	}
	if now.Sub(entry.observedAt) >= etagRetention {
		delete(s.items, key)
		return "", false
	}
	return entry.value, true
}

func (s *memoryETagStore) Put(key, etag string) {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(etag) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for existingKey, entry := range s.items {
		if now.Sub(entry.observedAt) >= etagRetention {
			delete(s.items, existingKey)
		}
	}
	if _, exists := s.items[key]; !exists && len(s.items) >= maxETagEntries {
		s.evictOldestETag()
	}
	s.items[key] = etagEntry{value: etag, observedAt: now}
}

func (s *memoryETagStore) evictOldestETag() {
	var oldestKey string
	var oldestAt time.Time
	for key, entry := range s.items {
		if oldestKey == "" || entry.observedAt.Before(oldestAt) {
			oldestKey = key
			oldestAt = entry.observedAt
		}
	}
	if oldestKey != "" {
		delete(s.items, oldestKey)
	}
}

// RequestContext carries all request and response state through the middleware chain.
type RequestContext struct {
	Context     context.Context
	Request     *Request
	Endpoint    Endpoint
	RouteKey    string
	UserKey     string
	Attempt     int
	HTTPRequest *http.Request
	Response    *HTTPResponse
	Metadata    *ResponseMetadata
}

// HTTPResponse is the bounded response captured by the ESI transport.
type HTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// RateLimit describes ESI route-bucket and error-limit response headers.
type RateLimit struct {
	Group               string
	Limit               int
	Remaining           int
	Used                int
	Window              time.Duration
	RetryAfter          time.Duration
	ErrorLimitRemaining int
	ErrorLimitReset     time.Duration
	Reset               time.Duration
	Present             bool
	RemainingPresent    bool
	ErrorLimitPresent   bool
}

// ResponseMetadata contains transport and response-header information.
type ResponseMetadata struct {
	StatusCode  int
	Headers     http.Header
	ETag        string
	RetryAfter  time.Duration
	RateLimit   RateLimit
	NotModified bool
}

// RoundTripper is the terminal or wrapped operation for one ESI request.
type RoundTripper func(*RequestContext) error

// Middleware wraps the next ESI request operation.
type Middleware func(next RoundTripper) RoundTripper

// WithMiddleware adds middleware to the ESI request pipeline.
func WithMiddleware(middleware ...Middleware) ClientOption {
	return func(options *clientOptions) error {
		for _, item := range middleware {
			if item == nil {
				return errors.New("ESI middleware must not be nil")
			}
			options.middleware = append(options.middleware, item)
		}
		return nil
	}
}

// RateLimitStore tracks ESI route groups, moving-window buckets, and cooldowns.
type RateLimitStore interface {
	Wait(ctx context.Context, routeKey, userKey string) error
	Observe(routeKey, userKey string, response *HTTPResponse, metadata *ResponseMetadata, now time.Time)
}

type memoryRateLimitStore struct {
	mu           sync.Mutex
	routeGroups  map[string]rateLimitRoute
	buckets      map[string]*rateLimitBucket
	errorUntil   map[string]time.Time
	lastPrunedAt time.Time
}

type rateLimitRoute struct {
	group      string
	observedAt time.Time
}

type rateLimitBucket struct {
	limit          int
	window         time.Duration
	remaining      int
	remainingKnown bool
	used           int
	observedAt     time.Time
	blockedUntil   time.Time
	charges        []time.Time
}

// NewMemoryRateLimitStore creates a process-local ESI bucket coordinator.
func NewMemoryRateLimitStore() RateLimitStore {
	return &memoryRateLimitStore{
		routeGroups: make(map[string]rateLimitRoute),
		buckets:     make(map[string]*rateLimitBucket),
		errorUntil:  make(map[string]time.Time),
	}
}

func (s *memoryRateLimitStore) Wait(ctx context.Context, routeKey, userKey string) error {
	if s == nil || routeKey == "" || userKey == "" {
		return nil
	}
	for {
		delay := s.reserve(routeKey, userKey, time.Now())
		if delay <= 0 {
			return nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *memoryRateLimitStore) Observe(routeKey, userKey string, response *HTTPResponse, metadata *ResponseMetadata, now time.Time) {
	if s == nil || response == nil || metadata == nil {
		return
	}
	snapshot := parseRateLimitHeaders(response.Headers)
	metadata.RateLimit = snapshot
	metadata.RetryAfter = snapshot.RetryAfter
	if snapshot.Group != "" {
		s.mu.Lock()
		s.pruneStale(now)
		if _, exists := s.routeGroups[routeKey]; !exists && len(s.routeGroups) >= maxRateLimitRoutes {
			s.evictOldestRoute()
		}
		s.routeGroups[routeKey] = rateLimitRoute{group: snapshot.Group, observedAt: now}
		if userKey != "" {
			s.recordErrorLimit(routeKey, userKey, response, &snapshot, now)
			s.recordBucket(userKey, response, &snapshot, now)
		}
		s.mu.Unlock()
		return
	}
	if userKey == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneStale(now)
	s.recordErrorLimit(routeKey, userKey, response, &snapshot, now)
	s.recordBucket(userKey, response, &snapshot, now)
}

func (s *memoryRateLimitStore) recordErrorLimit(routeKey, userKey string, response *HTTPResponse, snapshot *RateLimit, now time.Time) {
	rateLimited := response.StatusCode == statusESIRateLimited || response.StatusCode == http.StatusTooManyRequests
	if !snapshot.ErrorLimitPresent || (!rateLimited && snapshot.ErrorLimitRemaining != 0) {
		return
	}
	delay := clampDuration(snapshot.ErrorLimitReset, defaultRateLimitCooldown, maxRateLimitCooldown)
	if snapshot.RetryAfter > 0 {
		delay = clampDuration(snapshot.RetryAfter, defaultRateLimitCooldown, maxRateLimitCooldown)
	}
	key := rateLimitKey(routeKey, userKey)
	if _, exists := s.errorUntil[key]; !exists && len(s.errorUntil) >= maxRateLimitErrors {
		s.evictOldestError()
	}
	s.errorUntil[key] = now.Add(delay)
}

func (s *memoryRateLimitStore) recordBucket(userKey string, response *HTTPResponse, snapshot *RateLimit, now time.Time) {
	if snapshot.Group == "" || snapshot.Limit <= 0 || snapshot.Window <= 0 {
		return
	}
	bucketKey := rateLimitKey(snapshot.Group, userKey)
	bucket := s.buckets[bucketKey]
	if bucket == nil {
		if len(s.buckets) >= maxRateLimitBuckets {
			s.evictOldestBucket()
		}
		bucket = &rateLimitBucket{}
		s.buckets[bucketKey] = bucket
	}
	bucket.limit = snapshot.Limit
	bucket.window = snapshot.Window
	bucket.remaining = snapshot.Remaining
	bucket.remainingKnown = snapshot.RemainingPresent
	bucket.used = snapshot.Used
	bucket.observedAt = now
	if response.StatusCode == statusESIRateLimited || response.StatusCode == http.StatusTooManyRequests {
		delay := snapshot.RetryAfter
		if delay <= 0 {
			delay = snapshot.Window
		}
		bucket.blockedUntil = now.Add(delay)
	}
}

func (s *memoryRateLimitStore) reserve(routeKey, userKey string, now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneStale(now)

	key := rateLimitKey(routeKey, userKey)
	if until := s.errorUntil[key]; until.After(now) {
		return until.Sub(now)
	} else if !until.IsZero() {
		delete(s.errorUntil, key)
	}

	route, ok := s.routeGroups[routeKey]
	if !ok || route.group == "" {
		return 0
	}
	bucket := s.buckets[rateLimitKey(route.group, userKey)]
	if bucket == nil {
		return 0
	}
	if bucket.blockedUntil.After(now) {
		return bucket.blockedUntil.Sub(now)
	}
	if bucket.window <= 0 || bucket.observedAt.IsZero() {
		return 0
	}
	bucket.charges = pruneCharges(bucket.charges, now, bucket.window)
	if !bucket.observedAt.IsZero() && !now.Before(bucket.observedAt.Add(bucket.window)) {
		bucket.remaining = bucket.limit
		bucket.remainingKnown = false
		bucket.used = 0
		bucket.observedAt = now
	}
	if bucket.limit > 0 && len(bucket.charges) >= bucket.limit {
		return maxDuration(0, bucket.charges[0].Add(bucket.window).Sub(now))
	}
	if bucket.remainingKnown && bucket.remaining <= 0 && bucket.observedAt.Add(bucket.window).After(now) {
		return maxDuration(0, bucket.observedAt.Add(bucket.window).Sub(now))
	}
	bucket.charges = append(bucket.charges, now)
	if bucket.remainingKnown {
		bucket.remaining--
	}
	return 0
}

func parseRateLimitHeaders(headers http.Header) RateLimit {
	limit, window, limitOK := parseRateLimitWindow(headers.Get(headerRateLimitLimit))
	remaining, remainingOK := parseIntegerHeader(headers.Get(headerRateLimitRemaining))
	used, usedOK := parseIntegerHeader(headers.Get(headerRateLimitUsed))
	if limitOK && !remainingOK {
		remaining = limit
	}
	if limitOK && !usedOK {
		used = maxInt(0, limit-remaining)
	}
	errorRemaining, errorRemainingOK := parseIntegerHeader(headers.Get(headerErrorLimitRemain))
	errorReset, errorResetOK := parseSecondsHeader(headers.Get(headerErrorLimitReset))
	retryAfter := parseRetryAfter(headers.Get(headerRetryAfter), time.Now())
	return RateLimit{
		Group:               strings.TrimSpace(headers.Get(headerRateLimitGroup)),
		Limit:               limit,
		Remaining:           remaining,
		Used:                used,
		Window:              window,
		RetryAfter:          retryAfter,
		ErrorLimitRemaining: errorRemaining,
		ErrorLimitReset:     errorReset,
		Reset:               firstDuration(errorReset, window),
		Present:             limitOK || remainingOK || usedOK || errorRemainingOK || errorResetOK,
		RemainingPresent:    remainingOK,
		ErrorLimitPresent:   errorRemainingOK && errorResetOK,
	}
}

func parseRateLimitWindow(value string) (int, time.Duration, bool) {
	const expectedParts = 2

	parts := strings.Split(strings.ToLower(strings.TrimSpace(value)), "/")
	if len(parts) != expectedParts {
		return 0, 0, false
	}
	limit, limitOK := parsePositiveInteger(parts[0])
	windowValue := parts[1]
	windowMultiplier := time.Minute
	switch {
	case strings.HasSuffix(windowValue, "m"):
		windowValue = strings.TrimSuffix(windowValue, "m")
	case strings.HasSuffix(windowValue, "h"):
		windowValue = strings.TrimSuffix(windowValue, "h")
		windowMultiplier = time.Hour
	default:
		return 0, 0, false
	}
	windowSize, windowOK := parsePositiveInteger(windowValue)
	if !limitOK || !windowOK {
		return 0, 0, false
	}
	maximumWindowSize := int64(maxRateLimitCooldown / windowMultiplier)
	if int64(windowSize) > maximumWindowSize {
		return limit, maxRateLimitCooldown, true
	}
	return limit, time.Duration(windowSize) * windowMultiplier, true
}

func parsePositiveInteger(value string) (int, bool) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	return parsed, err == nil && parsed > 0
}

func parseIntegerHeader(value string) (int, bool) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	return parsed, err == nil
}

func parseSecondsHeader(value string) (time.Duration, bool) {
	seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || seconds < 0 {
		return 0, false
	}
	maximum := int64(maxRateLimitCooldown / time.Second)
	if seconds > maximum {
		return maxRateLimitCooldown, true
	}
	return time.Duration(seconds) * time.Second, true
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if duration, ok := parseSecondsHeader(value); ok {
		return duration
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return minDuration(when.Sub(now), maxRateLimitCooldown)
}

func (s *memoryRateLimitStore) pruneStale(now time.Time) {
	if !s.lastPrunedAt.IsZero() && now.Sub(s.lastPrunedAt) < rateLimitPruneInterval {
		return
	}
	for routeKey, route := range s.routeGroups {
		if now.Sub(route.observedAt) >= rateLimitStateRetention {
			delete(s.routeGroups, routeKey)
		}
	}
	for bucketKey, bucket := range s.buckets {
		if now.Sub(bucket.observedAt) >= rateLimitStateRetention && !bucket.blockedUntil.After(now) {
			delete(s.buckets, bucketKey)
		}
	}
	for key, until := range s.errorUntil {
		if !until.After(now) {
			delete(s.errorUntil, key)
		}
	}
	s.lastPrunedAt = now
}

func (s *memoryRateLimitStore) evictOldestRoute() {
	var oldestKey string
	var oldestAt time.Time
	for key, route := range s.routeGroups {
		if oldestKey == "" || route.observedAt.Before(oldestAt) {
			oldestKey = key
			oldestAt = route.observedAt
		}
	}
	if oldestKey != "" {
		delete(s.routeGroups, oldestKey)
	}
}

func (s *memoryRateLimitStore) evictOldestBucket() {
	var oldestKey string
	var oldestAt time.Time
	for key, bucket := range s.buckets {
		if oldestKey == "" || bucket.observedAt.Before(oldestAt) {
			oldestKey = key
			oldestAt = bucket.observedAt
		}
	}
	if oldestKey != "" {
		delete(s.buckets, oldestKey)
	}
}

func (s *memoryRateLimitStore) evictOldestError() {
	var oldestKey string
	var oldestAt time.Time
	for key, until := range s.errorUntil {
		if oldestKey == "" || until.Before(oldestAt) {
			oldestKey = key
			oldestAt = until
		}
	}
	if oldestKey != "" {
		delete(s.errorUntil, oldestKey)
	}
}

func pruneCharges(charges []time.Time, now time.Time, window time.Duration) []time.Time {
	cutoff := now.Add(-window)
	first := 0
	for first < len(charges) && !charges[first].After(cutoff) {
		first++
	}
	return charges[first:]
}

func rateLimitKey(first, second string) string {
	return first + "\x00" + second
}

func firstDuration(values ...time.Duration) time.Duration {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func clampDuration(value, fallback, maximum time.Duration) time.Duration {
	if value <= 0 {
		value = fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}
	return right
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

// ETagMiddleware adds conditional requests and records successful validators.
func ETagMiddleware(store ETagStore) Middleware {
	if store == nil {
		return func(next RoundTripper) RoundTripper { return next }
	}
	return func(next RoundTripper) RoundTripper {
		return func(request *RequestContext) error {
			addETagHeader(store, request)
			err := next(request)
			recordETagResponse(store, request)
			return err
		}
	}
}

func addETagHeader(store ETagStore, request *RequestContext) {
	if request == nil || request.Endpoint.CacheKey == "" || request.HTTPRequest == nil {
		return
	}
	etag, ok := store.Get(request.Endpoint.CacheKey)
	if ok {
		request.HTTPRequest.Header.Set(headerIfNoneMatch, etag)
	}
}

func recordETagResponse(store ETagStore, request *RequestContext) {
	if request == nil || request.Response == nil || request.Metadata == nil {
		return
	}
	etag := strings.TrimSpace(request.Response.Headers.Get(headerETag))
	request.Metadata.ETag = etag
	if request.Response.StatusCode < http.StatusOK ||
		request.Response.StatusCode >= http.StatusMultipleChoices ||
		etag == "" || request.Endpoint.CacheKey == "" {
		return
	}
	store.Put(request.Endpoint.CacheKey, etag)
}

// RateLimitMiddleware reserves local moving-window capacity before requests and
// records ESI limit headers after responses.
func RateLimitMiddleware(store RateLimitStore) Middleware {
	if store == nil {
		return func(next RoundTripper) RoundTripper { return next }
	}
	return func(next RoundTripper) RoundTripper {
		return func(request *RequestContext) error {
			if request == nil {
				return errors.New("ESI request context must not be nil")
			}
			if err := store.Wait(request.Context, request.RouteKey, request.UserKey); err != nil {
				return err
			}
			err := next(request)
			store.Observe(request.RouteKey, request.UserKey, request.Response, request.Metadata, time.Now())
			return err
		}
	}
}
