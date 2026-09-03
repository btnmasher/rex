package token

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/btnmasher/rex/internal/tokenexport"
)

type exportSource struct {
	mu           sync.Mutex
	scoped       tokenexport.Response
	corpCalls    int
	fetchStarted chan struct{}
	fetchRelease chan struct{}
	startedOnce  sync.Once
}

func (s *exportSource) ListCorporations(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []string{"100"}, nil
}

func (s *exportSource) FetchCorporation(ctx context.Context, _ string) (tokenexport.Response, error) {
	s.mu.Lock()
	s.corpCalls++
	scoped := s.scoped
	started := s.fetchStarted
	release := s.fetchRelease
	s.mu.Unlock()
	if started != nil {
		s.startedOnce.Do(func() { close(started) })
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return tokenexport.Response{}, ctx.Err()
		}
	}
	return scoped, nil
}

func TestProviderLoadsSnapshotAndRoundRobinsAccessTokens(t *testing.T) {
	source := &exportSource{}
	provider, err := NewProvider(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.InstallCorporation(context.Background(), testGroup("1", "2")); err != nil {
		t.Fatal(err)
	}
	first, err := provider.NextAccessToken(context.Background(), "100")
	if err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.lastCorporationPoll["100"] = time.Now().Add(-notificationTokenCooldown - time.Second)
	provider.mu.Unlock()
	second, err := provider.NextAccessToken(context.Background(), "100")
	if err != nil {
		t.Fatal(err)
	}
	if first.CharacterID != "1" || second.CharacterID != "2" || first.AccessToken != "access-1" || second.AccessToken != "access-2" {
		t.Fatalf("credentials = %+v, %+v", first, second)
	}
}

func TestProviderReportsConfiguredTokenAvailability(t *testing.T) {
	provider, err := NewProvider(&exportSource{})
	if err != nil {
		t.Fatal(err)
	}
	if provider.HasConfiguredTokens("100") {
		t.Fatal("expected unloaded corporation to have no configured tokens")
	}
	if err := provider.InstallCorporation(context.Background(), testGroup()); err != nil {
		t.Fatal(err)
	}
	if provider.HasConfiguredTokens("100") {
		t.Fatal("expected empty token group to have no configured tokens")
	}
	if err := provider.InstallCorporation(context.Background(), testGroup("1")); err != nil {
		t.Fatal(err)
	}
	if !provider.HasConfiguredTokens("100") {
		t.Fatal("expected non-empty token group to have configured tokens")
	}
	if provider.TokenCount("100") != 1 {
		t.Fatalf("expected one configured token, got %d", provider.TokenCount("100"))
	}
}

func TestProviderSerializesCorporationRotation(t *testing.T) {
	source := &exportSource{}
	provider, err := NewProvider(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.InstallCorporation(context.Background(), testGroup("1", "2")); err != nil {
		t.Fatal(err)
	}
	results := make(chan Credential, 2)
	provider.notificationCooldown = 0
	provider.minimumPollInterval = 0
	var wait sync.WaitGroup
	for range 2 {
		wait.Go(func() {
			credential, callErr := provider.NextAccessToken(context.Background(), "100")
			if callErr != nil {
				t.Errorf("next access token: %v", callErr)
				return
			}
			results <- credential
		})
	}
	wait.Wait()
	close(results)
	characters := make(map[string]struct{})
	for credential := range results {
		characters[credential.CharacterID] = struct{}{}
	}
	if len(characters) != 2 {
		t.Fatalf("concurrent rotation selected characters = %#v", characters)
	}
}

func TestProviderDoesNotReuseNotificationTokenDuringCooldown(t *testing.T) {
	source := &exportSource{}
	provider, err := NewProvider(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.InstallCorporation(context.Background(), testGroup("1", "2")); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.NextAccessToken(context.Background(), "100"); err != nil {
		t.Fatal(err)
	}
	_, err = provider.NextAccessToken(context.Background(), "100")
	if !errors.Is(err, ErrNotificationCooldown) {
		t.Fatalf("expected corporation cadence cooldown, got %v", err)
	}
	var cooldownErr *CooldownError
	if !errors.As(err, &cooldownErr) || cooldownErr.Reason != "corporation_cadence" || cooldownErr.RetryAfter <= 0 {
		t.Fatalf("unexpected corporation cadence details: %#v", cooldownErr)
	}

	provider.mu.Lock()
	provider.lastCorporationPoll["100"] = time.Now().Add(-notificationTokenCooldown)
	provider.mu.Unlock()
	second, err := provider.NextAccessToken(context.Background(), "100")
	if err != nil {
		t.Fatal(err)
	}
	if second.CharacterID != "2" {
		t.Fatalf("expected second token after cadence delay, got %s", second.CharacterID)
	}
	if _, err := provider.NextAccessToken(context.Background(), "100"); !errors.Is(err, ErrNotificationCooldown) {
		t.Fatalf("expected cooldown refusal after second token, got %v", err)
	}
}

func TestProviderReportsWhenAllTokenIdentitiesAreCoolingDown(t *testing.T) {
	provider, err := NewProvider(&exportSource{})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.InstallCorporation(context.Background(), testGroup("1", "2")); err != nil {
		t.Fatal(err)
	}
	provider.minimumPollInterval = 0
	if _, err := provider.NextAccessToken(context.Background(), "100"); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.lastCorporationPoll["100"] = time.Now().Add(-notificationTokenCooldown - time.Second)
	provider.mu.Unlock()
	if _, err := provider.NextAccessToken(context.Background(), "100"); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.lastCorporationPoll["100"] = time.Now().Add(-notificationTokenCooldown - time.Second)
	provider.mu.Unlock()
	_, err = provider.NextAccessToken(context.Background(), "100")
	if !errors.Is(err, ErrNotificationCooldown) {
		t.Fatalf("expected token identity cooldown, got %v", err)
	}
	var cooldownErr *CooldownError
	if !errors.As(err, &cooldownErr) || cooldownErr.Reason != "all_token_identities_cooling_down" || cooldownErr.RetryAfter <= 0 {
		t.Fatalf("unexpected token identity cooldown details: %#v", cooldownErr)
	}
}

func TestProviderDoesNotSmoothCorporationsWithTenOrMoreTokens(t *testing.T) {
	provider, err := NewProvider(&exportSource{})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.InstallCorporation(context.Background(), testGroup("1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15")); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.NextAccessToken(context.Background(), "100"); err != nil {
		t.Fatal(err)
	}
	second, err := provider.NextAccessToken(context.Background(), "100")
	if err != nil {
		t.Fatalf("expected next token without corporation smoothing: %v", err)
	}
	if second.CharacterID != "2" {
		t.Fatalf("selected character = %s, want 2", second.CharacterID)
	}
}

func TestProviderPreservesExistingTokenCooldownAcrossFullRefresh(t *testing.T) {
	source := &exportSource{}
	provider, err := NewProvider(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.InstallCorporation(context.Background(), testGroup("1")); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.NextAccessToken(context.Background(), "100"); err != nil {
		t.Fatal(err)
	}

	if err := provider.InstallCorporation(context.Background(), testGroup("1", "2")); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	_, existingHasCooldown := provider.lastNotificationAt[accessKey{corporationID: "100", characterID: "1"}]
	_, newHasCooldown := provider.lastNotificationAt[accessKey{corporationID: "100", characterID: "2"}]
	provider.mu.Unlock()
	if !existingHasCooldown || newHasCooldown {
		t.Fatalf("cooldowns after refresh: existing=%t new=%t", existingHasCooldown, newHasCooldown)
	}
}

func TestProviderRefreshesAccessTokenWithScopedExport(t *testing.T) {
	source := &exportSource{
		scoped: tokenexport.Response{Corporations: []tokenexport.CorporationTokenGroup{testGroup("replacement")}},
	}
	provider, err := NewProvider(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.InstallCorporation(context.Background(), testGroup("1")); err != nil {
		t.Fatal(err)
	}
	credential, err := provider.RefreshAccessToken(context.Background(), "100", "1")
	if err != nil {
		t.Fatal(err)
	}
	if credential.CharacterID != "replacement" || credential.AccessToken != "access-replacement" {
		t.Fatalf("replacement credential = %+v", credential)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.corpCalls != 1 {
		t.Fatalf("scoped refresh calls = %d", source.corpCalls)
	}
}

func TestProviderCoalescesConcurrentForcedRefreshes(t *testing.T) {
	source := &exportSource{
		scoped:       tokenexport.Response{Corporations: []tokenexport.CorporationTokenGroup{testGroup("replacement")}},
		fetchStarted: make(chan struct{}),
		fetchRelease: make(chan struct{}),
	}
	provider, err := NewProvider(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.InstallCorporation(context.Background(), testGroup("initial")); err != nil {
		t.Fatal(err)
	}

	results := make(chan error, 8)
	for range 8 {
		go func() { results <- provider.RefreshCorporation(context.Background(), "100") }()
	}
	select {
	case <-source.fetchStarted:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	time.Sleep(10 * time.Millisecond)
	close(source.fetchRelease)
	for range 8 {
		if err := <-results; err != nil {
			t.Fatalf("forced refresh: %v", err)
		}
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.corpCalls != 1 {
		t.Fatalf("coalesced refresh calls = %d, want 1", source.corpCalls)
	}
}

func TestProviderSkipsScheduledRefreshAfterRecentAdHocRefresh(t *testing.T) {
	source := &exportSource{
		scoped: tokenexport.Response{Corporations: []tokenexport.CorporationTokenGroup{testGroup("replacement")}},
	}
	provider, err := NewProvider(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.InstallCorporation(context.Background(), testGroup("initial")); err != nil {
		t.Fatal(err)
	}

	refreshed, err := provider.RefreshCorporationIfDue(context.Background(), "100", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed {
		t.Fatal("expected recent scheduled refresh to be skipped")
	}
	if err := provider.RefreshCorporation(context.Background(), "100"); err != nil {
		t.Fatal(err)
	}
	refreshed, err = provider.RefreshCorporationIfDue(context.Background(), "100", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed {
		t.Fatal("expected recent ad-hoc refresh to suppress scheduled refresh")
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.corpCalls != 1 {
		t.Fatalf("scoped refresh calls = %d, want 1", source.corpCalls)
	}
}

func TestProviderRemovesCorporationsThatAreNoLongerEligible(t *testing.T) {
	provider, err := NewProvider(&exportSource{})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.InstallCorporation(context.Background(), testGroup("1")); err != nil {
		t.Fatal(err)
	}
	provider.SetEligibleCorporations([]string{"200"})
	if _, err := provider.NextAccessToken(context.Background(), "100"); !errors.Is(err, ErrNoUsableToken) {
		t.Fatalf("expected stale corporation to be removed, got %v", err)
	}
	if provider.corporationLocks.len() != 0 {
		t.Fatalf("corporation locks retained after use: %d", provider.corporationLocks.len())
	}
}

func TestKeyedLockerHonorsCanceledWaiters(t *testing.T) {
	locker := newKeyedLocker()
	release, err := locker.Acquire(context.Background(), "100")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	acquireResult := make(chan error, 1)
	go func() {
		_, acquireErr := locker.Acquire(ctx, "100")
		acquireResult <- acquireErr
	}()

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		locker.mu.Lock()
		lock := locker.locks["100"]
		references := 0
		if lock != nil {
			references = lock.references
		}
		locker.mu.Unlock()
		if references == 2 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("canceled waiter did not register")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	select {
	case err := <-acquireResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("acquire error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter remained blocked")
	}

	if locker.len() != 1 {
		t.Fatalf("keyed locks after cancellation = %d, want 1", locker.len())
	}
}

func testGroup(characterIDs ...string) tokenexport.CorporationTokenGroup {
	group := tokenexport.CorporationTokenGroup{CorporationID: "100", CorporationName: "Corp"}
	for i, characterID := range characterIDs {
		group.Tokens = append(group.Tokens, tokenexport.AccessTokenRecord{
			CharacterID:   characterID,
			CharacterName: "Pilot",
			UserID:        uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1)),
			Role:          "member",
			AccessToken:   "access-" + characterID,
		})
	}
	return group
}
