// Package token owns the in-memory auth-next access-token pool.
package token

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/btnmasher/rex/internal/tokenexport"
)

const (
	notificationTokenCooldown = 10 * time.Minute
	maxTokenPoolSize          = 64
)

// ExportSource supplies validated access-token groups from auth-next.
type ExportSource interface {
	ListCorporations(context.Context) ([]string, error)
	FetchCorporation(context.Context, string) (tokenexport.Response, error)
}

// Credential is the selected EVE identity and access token for one ESI request.
type Credential struct {
	CharacterID string
	AccessToken string
}

// Provider manages corporation-scoped access tokens and notification rotation.
type Provider struct {
	source ExportSource

	mu                   sync.Mutex
	groups               map[string]*corporationGroup
	lastNotificationAt   map[accessKey]time.Time
	lastCorporationPoll  map[string]time.Time
	lastRefreshedAt      map[string]time.Time
	notificationCooldown time.Duration
	minimumPollInterval  time.Duration
	corporationLocks     *keyedLocker
	eligibleCorporations map[string]struct{}
	eligibilityKnown     bool
	refreshes            map[string]*refreshCall
}

type corporationGroup struct {
	tokens    []tokenRecord
	nextIndex int
}

type tokenRecord struct {
	characterID string
	accessToken string
	expiresAt   time.Time
}

type accessKey struct {
	corporationID string
	characterID   string
}

type refreshCall struct {
	done      chan struct{}
	refreshed bool
	err       error
}

// NewProvider creates an access-token provider backed by the auth-next export source.
// pollInterval controls how frequently a corporation may be polled and is used
// to determine when token rotation must be stretched to honor identity cooldowns.
func NewProvider(source ExportSource, pollInterval time.Duration) (*Provider, error) {
	if source == nil {
		return nil, errors.New("token provider source is required")
	}
	if pollInterval <= 0 {
		return nil, errors.New("token provider poll interval must be positive")
	}
	provider := &Provider{
		source:               source,
		groups:               make(map[string]*corporationGroup),
		lastNotificationAt:   make(map[accessKey]time.Time),
		lastCorporationPoll:  make(map[string]time.Time),
		lastRefreshedAt:      make(map[string]time.Time),
		notificationCooldown: notificationTokenCooldown,
		minimumPollInterval:  pollInterval,
		corporationLocks:     newKeyedLocker(),
		eligibleCorporations: make(map[string]struct{}),
		refreshes:            make(map[string]*refreshCall),
	}
	return provider, nil
}

// NextAccessToken selects the next corporation credential in round-robin order.
func (p *Provider) NextAccessToken(ctx context.Context, corporationID string) (Credential, error) {
	if p == nil {
		return Credential{}, errors.New("token provider is unavailable")
	}
	if ctx == nil {
		return Credential{}, errors.New("token selection context is required")
	}
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	release, err := p.corporationLocks.Acquire(ctx, corporationID)
	if err != nil {
		return Credential{}, err
	}
	defer release()
	return p.nextAccessTokenSerial(corporationID)
}

// RefreshCorporation fetches and installs a fresh access-token set for a corporation.
func (p *Provider) RefreshCorporation(ctx context.Context, corporationID string) error {
	if p == nil {
		return errors.New("token provider is unavailable")
	}
	if ctx == nil {
		return errors.New("token refresh context is required")
	}
	if strings.TrimSpace(corporationID) == "" {
		return ErrNoUsableToken
	}
	_, err := p.refreshCorporation(ctx, corporationID, true, 0)
	return err
}

// RefreshCorporationIfDue refreshes a corporation only when its last successful
// refresh is older than minimumAge. Concurrent refreshes share one fetch.
func (p *Provider) RefreshCorporationIfDue(ctx context.Context, corporationID string, minimumAge time.Duration) (bool, error) {
	if p == nil {
		return false, errors.New("token provider is unavailable")
	}
	if ctx == nil {
		return false, errors.New("token refresh context is required")
	}
	if strings.TrimSpace(corporationID) == "" {
		return false, ErrNoUsableToken
	}
	return p.refreshCorporation(ctx, corporationID, false, minimumAge)
}

// RefreshAccessToken fetches a fresh access-token set for a corporation after an ESI authentication failure.
func (p *Provider) RefreshAccessToken(ctx context.Context, corporationID, characterID string) (Credential, error) {
	if p == nil {
		return Credential{}, errors.New("token provider is unavailable")
	}
	if ctx == nil {
		return Credential{}, errors.New("token refresh context is required")
	}
	if strings.TrimSpace(corporationID) == "" || strings.TrimSpace(characterID) == "" {
		return Credential{}, ErrNoUsableToken
	}
	if err := p.RefreshCorporation(ctx, corporationID); err != nil {
		return Credential{}, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if credential, ok := p.refreshedCredentialLocked(corporationID, characterID); ok {
		return credential, nil
	}
	return Credential{}, fmt.Errorf("%w: corporation %s", ErrNoUsableToken, corporationID)
}

// SetEligibleCorporations removes credentials for corporations no longer returned by auth-next.
func (p *Provider) SetEligibleCorporations(corporationIDs []string) {
	eligible := make(map[string]struct{}, len(corporationIDs))
	for _, corporationID := range corporationIDs {
		eligible[corporationID] = struct{}{}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.eligibleCorporations = eligible
	p.eligibilityKnown = true
	for corporationID := range p.groups {
		if _, exists := eligible[corporationID]; exists {
			continue
		}
		p.clearNotificationStateLocked(corporationID)
		delete(p.groups, corporationID)
		delete(p.lastRefreshedAt, corporationID)
	}
}

// HasConfiguredTokens reports whether a corporation has at least one cached token record.
// The record may be expired; callers can use NextAccessToken to obtain the detailed state.
func (p *Provider) HasConfiguredTokens(corporationID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	group := p.groups[corporationID]
	return group != nil && len(group.tokens) > 0
}

// TokenCount reports the number of cached token records for a corporation.
// It does not expose token values or indicate whether the records are usable.
func (p *Provider) TokenCount(corporationID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	group := p.groups[corporationID]
	if group == nil {
		return 0
	}
	return len(group.tokens)
}

// HasUsableTokens reports whether at least one non-expired access token is cached.
func (p *Provider) HasUsableTokens() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, group := range p.groups {
		for _, record := range group.tokens {
			if record.accessToken == "" || (!record.expiresAt.IsZero() && !now.Before(record.expiresAt)) {
				continue
			}
			return true
		}
	}
	return false
}

// InstallCorporation replaces one corporation's token set while preserving cooldowns by character ID.
// The context controls waiting for the corporation-scoped installation lock.
func (p *Provider) InstallCorporation(ctx context.Context, group tokenexport.CorporationTokenGroup) error {
	if ctx == nil {
		return errors.New("token installation context is required")
	}
	if strings.TrimSpace(group.CorporationID) == "" {
		return ErrNoUsableToken
	}
	release, err := p.corporationLocks.Acquire(ctx, group.CorporationID)
	if err != nil {
		return err
	}
	defer release()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.eligibilityKnown {
		if _, eligible := p.eligibleCorporations[group.CorporationID]; !eligible {
			return fmt.Errorf("%w: corporation %s is no longer eligible", ErrNoUsableToken, group.CorporationID)
		}
	}
	p.installCorporationLocked(group)
	return nil
}

func (p *Provider) refreshedCredentialLocked(corporationID, characterID string) (Credential, bool) {
	group := p.groups[corporationID]
	if group == nil {
		return Credential{}, false
	}
	for _, record := range group.tokens {
		if record.characterID == characterID && (record.expiresAt.IsZero() || time.Now().Before(record.expiresAt)) {
			return credentialFromRecord(record), true
		}
	}
	for offset := range group.tokens {
		record := group.tokens[(group.nextIndex+offset)%len(group.tokens)]
		if record.accessToken != "" && (record.expiresAt.IsZero() || time.Now().Before(record.expiresAt)) {
			return credentialFromRecord(record), true
		}
	}
	return Credential{}, false
}

func (p *Provider) installCorporationLocked(group tokenexport.CorporationTokenGroup) {
	oldGroup := p.groups[group.CorporationID]
	newGroup := newCorporationGroup(group)
	p.groups[group.CorporationID] = newGroup
	p.lastRefreshedAt[group.CorporationID] = time.Now().UTC()
	if oldGroup != nil {
		p.replaceNotificationStateLocked(group.CorporationID, oldGroup, newGroup)
	}
}

func (p *Provider) refreshCorporation(ctx context.Context, corporationID string, force bool, minimumAge time.Duration) (bool, error) {
	p.mu.Lock()
	if call, ok := p.refreshes[corporationID]; ok {
		done := call.done
		p.mu.Unlock()
		select {
		case <-done:
			if call.err != nil || call.refreshed || !force {
				return call.refreshed, call.err
			}
			return p.refreshCorporation(ctx, corporationID, true, 0)
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	if !force && p.recentlyRefreshedLocked(corporationID, minimumAge) {
		p.mu.Unlock()
		return false, nil
	}
	call := &refreshCall{done: make(chan struct{})}
	p.refreshes[corporationID] = call
	p.mu.Unlock()

	refreshed, err := p.fetchAndInstallCorporation(ctx, corporationID)
	p.mu.Lock()
	delete(p.refreshes, corporationID)
	call.refreshed = refreshed
	call.err = err
	close(call.done)
	p.mu.Unlock()
	return refreshed, err
}

func (p *Provider) fetchAndInstallCorporation(ctx context.Context, corporationID string) (bool, error) {
	response, err := p.source.FetchCorporation(ctx, corporationID)
	if err != nil {
		return false, err
	}
	group, found := findCorporation(response, corporationID)
	if !found {
		return false, fmt.Errorf("%w: corporation %s was not returned", ErrNoUsableToken, corporationID)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.eligibilityKnown {
		if _, eligible := p.eligibleCorporations[corporationID]; !eligible {
			return false, fmt.Errorf("%w: corporation %s is no longer eligible", ErrNoUsableToken, corporationID)
		}
	}
	p.installCorporationLocked(group)
	return true, nil
}

func (p *Provider) recentlyRefreshedLocked(corporationID string, minimumAge time.Duration) bool {
	if minimumAge <= 0 {
		return false
	}
	lastRefreshedAt := p.lastRefreshedAt[corporationID]
	return !lastRefreshedAt.IsZero() && time.Now().UTC().Before(lastRefreshedAt.Add(minimumAge))
}

func findCorporation(response tokenexport.Response, corporationID string) (tokenexport.CorporationTokenGroup, bool) {
	for i := range response.Corporations {
		if response.Corporations[i].CorporationID == corporationID {
			return response.Corporations[i], true
		}
	}
	return tokenexport.CorporationTokenGroup{}, false
}

func (p *Provider) nextAccessTokenSerial(corporationID string) (Credential, error) {
	if retryAfter := p.corporationPollRetryAfter(corporationID); retryAfter > 0 {
		return Credential{}, &CooldownError{
			CorporationID: corporationID,
			Reason:        "corporation_cadence",
			RetryAfter:    retryAfter,
		}
	}
	selection := p.selectToken(corporationID)
	if !selection.found {
		if selection.coolingDown {
			return Credential{}, &CooldownError{
				CorporationID: corporationID,
				Reason:        "all_token_identities_cooling_down",
				RetryAfter:    selection.retryAfter,
			}
		}
		return Credential{}, &NoUsableTokenError{
			CorporationID: corporationID,
			Reason:        selection.reason,
			TokenCount:    selection.tokenCount,
			EmptyCount:    selection.emptyCount,
			ExpiredCount:  selection.expiredCount,
		}
	}

	p.markNotificationUse(corporationID, selection.record.characterID)
	p.advanceAfterSuccess(corporationID, selection.group, selection.index)
	return credentialFromRecord(selection.record), nil
}

type tokenSelection struct {
	record       tokenRecord
	index        int
	group        *corporationGroup
	retryAfter   time.Duration
	tokenCount   int
	emptyCount   int
	expiredCount int
	reason       string
	found        bool
	coolingDown  bool
}

func (p *Provider) selectToken(corporationID string) tokenSelection {
	p.mu.Lock()
	defer p.mu.Unlock()
	group := p.groups[corporationID]
	if group == nil {
		return tokenSelection{reason: "corporation_not_loaded"}
	}
	if len(group.tokens) == 0 {
		return tokenSelection{reason: "no_tokens_cached"}
	}
	now := time.Now()
	coolingDown := false
	var retryAfter time.Duration
	emptyCount := 0
	expiredCount := 0
	for offset := range group.tokens {
		index := (group.nextIndex + offset) % len(group.tokens)
		record := group.tokens[index]
		if record.accessToken == "" {
			emptyCount++
			continue
		}
		if !record.expiresAt.IsZero() && !now.Before(record.expiresAt) {
			expiredCount++
			continue
		}
		key := accessKey{corporationID: corporationID, characterID: record.characterID}
		if lastUsed := p.lastNotificationAt[key]; !lastUsed.IsZero() && now.Before(lastUsed.Add(p.notificationCooldown)) {
			coolingDown = true
			remaining := lastUsed.Add(p.notificationCooldown).Sub(now)
			if retryAfter <= 0 || remaining < retryAfter {
				retryAfter = remaining
			}
			continue
		}
		return tokenSelection{record: record, index: index, group: group, found: true}
	}
	return tokenSelection{
		tokenCount:   len(group.tokens),
		emptyCount:   emptyCount,
		expiredCount: expiredCount,
		reason:       unusableTokenReason(len(group.tokens), emptyCount, expiredCount),
		coolingDown:  coolingDown,
		retryAfter:   retryAfter,
	}
}

func unusableTokenReason(tokenCount, emptyCount, expiredCount int) string {
	switch {
	case tokenCount == 0:
		return "no_tokens_cached"
	case emptyCount == tokenCount:
		return "all_tokens_empty"
	case expiredCount == tokenCount:
		return "all_tokens_expired"
	default:
		return "all_tokens_unusable"
	}
}

func (p *Provider) corporationPollRetryAfter(corporationID string) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	group := p.groups[corporationID]
	if group == nil || len(group.tokens) == 0 {
		return 0
	}
	if len(group.tokens) >= p.requiredTokenCount() {
		return 0
	}
	cadence := p.notificationCooldown / time.Duration(len(group.tokens))
	cadence = max(cadence, p.minimumPollInterval)
	lastPoll := p.lastCorporationPoll[corporationID]
	if lastPoll.IsZero() {
		return 0
	}
	retryAfter := time.Until(lastPoll.Add(cadence))
	return max(retryAfter, 0)
}

func (p *Provider) requiredTokenCount() int {
	if p == nil || p.minimumPollInterval <= 0 {
		return 1
	}
	required := int(p.notificationCooldown / p.minimumPollInterval)
	if p.notificationCooldown%p.minimumPollInterval != 0 {
		required++
	}
	return max(required, 1)
}

func (p *Provider) markNotificationUse(corporationID, characterID string) {
	p.mu.Lock()
	now := time.Now()
	p.lastNotificationAt[accessKey{corporationID: corporationID, characterID: characterID}] = now
	p.lastCorporationPoll[corporationID] = now
	p.mu.Unlock()
}

func (p *Provider) advanceAfterSuccess(corporationID string, selectedGroup *corporationGroup, selectedIndex int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	group := p.groups[corporationID]
	if group == nil || group != selectedGroup || selectedIndex < 0 || selectedIndex >= len(group.tokens) {
		return
	}
	group.nextIndex = (selectedIndex + 1) % len(group.tokens)
}

func (p *Provider) replaceNotificationStateLocked(corporationID string, oldGroup, newGroup *corporationGroup) {
	preserved := make(map[string]time.Time)
	newCharacters := make(map[string]struct{}, len(newGroup.tokens))
	for _, record := range newGroup.tokens {
		newCharacters[record.characterID] = struct{}{}
	}
	for _, record := range oldGroup.tokens {
		if _, exists := newCharacters[record.characterID]; !exists {
			continue
		}
		key := accessKey{corporationID: corporationID, characterID: record.characterID}
		if lastUsed, exists := p.lastNotificationAt[key]; exists {
			preserved[record.characterID] = lastUsed
		}
	}
	lastPoll, hadLastPoll := p.lastCorporationPoll[corporationID]
	p.clearNotificationStateLocked(corporationID)
	for characterID, lastUsed := range preserved {
		p.lastNotificationAt[accessKey{corporationID: corporationID, characterID: characterID}] = lastUsed
	}
	if hadLastPoll {
		p.lastCorporationPoll[corporationID] = lastPoll
	}
}

func (p *Provider) clearNotificationStateLocked(corporationID string) {
	for key := range p.lastNotificationAt {
		if key.corporationID == corporationID {
			delete(p.lastNotificationAt, key)
		}
	}
	delete(p.lastCorporationPoll, corporationID)
}

func newCorporationGroup(group tokenexport.CorporationTokenGroup) *corporationGroup {
	tokens := make([]tokenRecord, 0, len(group.Tokens))
	for _, token := range group.Tokens {
		tokens = append(tokens, tokenRecord{
			characterID: token.CharacterID,
			accessToken: token.AccessToken,
			expiresAt:   token.ExpiresAt,
		})
	}
	return &corporationGroup{tokens: tokens}
}

func credentialFromRecord(record tokenRecord) Credential {
	return Credential{
		CharacterID: record.characterID,
		AccessToken: record.accessToken,
	}
}

var _ ExportSource = (*tokenexport.Client)(nil)
