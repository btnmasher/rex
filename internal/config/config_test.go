package config

import (
	"os"
	"path/filepath"
	"testing"
)

func setRequiredEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://localhost/rex")
	t.Setenv("AUTH_NEXT_TOKEN_EXPORT_BASE_URL", "http://localhost:3000")
	t.Setenv("AUTH_NEXT_TOKEN_EXPORT_BEARER_TOKEN", "test-bearer")
	t.Setenv("EVE_SSO_CLIENT_ID", "test-client")
	writeDestinationsFile(t, `[{"name":"test-alerts","webhookUrls":["https://discord.com/api/webhooks/123/token"],"alertTypes":["all"]}]`)
}

func writeDestinationsFile(t *testing.T, contents string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "alert-destinations.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write destinations file: %v", err)
	}
	t.Setenv("ALERT_DESTINATIONS_FILE", path)
}

func TestLoadTokenExportConfiguration(t *testing.T) {
	setRequiredEnvironment(t)

	config, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if config.AuthNextTokenExportBaseURL != "http://localhost:3000" || config.AuthNextTokenExportBearerToken != "test-bearer" {
		t.Fatalf("unexpected token export configuration: %#v", config)
	}
}

func TestLoadRejectsMissingTokenExportBearer(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("AUTH_NEXT_TOKEN_EXPORT_BEARER_TOKEN", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected missing bearer token error")
	}
}

func TestLoadAlertDestinations(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, `[
		{"name":"structure-alerts","webhookUrls":["https://discord.com/api/webhooks/123/token"],"alertTypes":[" structures.combat.under_attack ","sovereignty.events"]}
	]`)
	t.Setenv("DISCORD_OVERRIDE_SENDER_NAME", "Rex")
	t.Setenv("DISCORD_OVERRIDE_SENDER_AVATAR_URL", "https://images.example.invalid/rex.png")
	t.Setenv("LOG_PRETTY", "true")
	t.Setenv("LOG_PAYLOADS", "true")
	t.Setenv("NOTIFICATION_STATE_SQLITE_PATH", "/tmp/rex-state.sqlite")

	config, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(config.AlertDestinations) != 1 {
		t.Fatalf("unexpected destinations: %#v", config.AlertDestinations)
	}
	destination := config.AlertDestinations[0]
	if destination.Name != "structure-alerts" || len(destination.WebhookURLs) != 1 || destination.WebhookURLs[0] != "https://discord.com/api/webhooks/123/token" ||
		len(destination.AlertTypes) != 2 || destination.AlertTypes[0] != "structures.combat.under_attack" || destination.AlertTypes[1] != "sovereignty.events" {
		t.Fatalf("unexpected destination: %#v", destination)
	}
	if config.DiscordOverrideSenderName != "Rex" || config.DiscordOverrideSenderAvatarURL != "https://images.example.invalid/rex.png" {
		t.Fatalf("unexpected Discord sender configuration: %#v", config)
	}
	if config.DiscordShowEntityIDs {
		t.Fatal("expected entity IDs to be hidden by default")
	}
	if !config.LogPretty {
		t.Fatal("expected pretty logging to be enabled")
	}
	if !config.LogPayloads {
		t.Fatal("expected payload logging to be enabled")
	}
	if config.NotificationStateSQLitePath != "/tmp/rex-state.sqlite" {
		t.Fatalf("unexpected notification state path: %q", config.NotificationStateSQLitePath)
	}
}

func TestLoadAlertDestinationSupportsMultipleWebhookURLs(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, `[{"name":"structure-alerts","webhookUrls":["https://discord.com/api/webhooks/123/token-a","https://discord.com/api/webhooks/456/token-b"],"alertTypes":["structures.combat"]}]`)

	config, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	destination := config.AlertDestinations[0]
	if len(destination.WebhookURLs) != 2 || destination.WebhookURLs[0] != "https://discord.com/api/webhooks/123/token-a" || destination.WebhookURLs[1] != "https://discord.com/api/webhooks/456/token-b" {
		t.Fatalf("unexpected webhook URLs: %#v", destination.WebhookURLs)
	}
}

func TestLoadSupportsDiscordEntityIDVisibility(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("DISCORD_SHOW_ENTITY_IDS", "true")

	config, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !config.DiscordShowEntityIDs {
		t.Fatal("expected entity IDs to be enabled")
	}
}

func TestLoadCapsPollLookbehind(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("POLL_LOOKBEHIND", "1h")

	config, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if config.PollLookbehind != maxPollLookbehind {
		t.Fatalf("poll lookbehind = %s, want %s", config.PollLookbehind, maxPollLookbehind)
	}
}

func TestLoadRejectsInvalidDiscordEntityIDVisibility(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("DISCORD_SHOW_ENTITY_IDS", "sometimes")

	_, err := Load()
	if err == nil {
		t.Fatal("expected invalid entity ID visibility error")
	}
}

func TestLoadRejectsDuplicateDestinationNames(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, `[
		{"name":"alerts","webhookUrls":["https://discord.com/api/webhooks/123/token"],"alertTypes":["structures.combat.under_attack"]},
		{"name":"alerts","webhookUrls":["https://discord.com/api/webhooks/456/token"],"alertTypes":["structures.combat.destroyed"]}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected duplicate destination name error")
	}
}

func TestLoadAlertDestinationsAllShorthand(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, `[{"name":"all-alerts","webhookUrls":["https://discord.com/api/webhooks/123/token"],"alertTypes":[" ALL "]}]`)

	config, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(config.AlertDestinations) != 1 || len(config.AlertDestinations[0].AlertTypes) != 1 || config.AlertDestinations[0].AlertTypes[0] != "all" {
		t.Fatalf("expected all shorthand to enable every category, got %#v", config.AlertDestinations)
	}
}

func TestLoadAlertDestinationsSupportsGroupsAndLeafExclusions(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, `[{
		"name":"skyhook-alerts",
		"webhookUrls":["https://discord.com/api/webhooks/123/token"],
		"alertTypes":[" SKYHOOKS.* "],
		"excludeAlertTypes":[" SKYHOOKS.LOST_SHIELDS "],
		"excludeStructureTypeIDs":[" 00081080 "]
	}]`)

	config, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	destination := config.AlertDestinations[0]
	if len(destination.AlertTypes) != 1 || destination.AlertTypes[0] != "skyhooks" {
		t.Fatalf("unexpected included alert types: %#v", destination.AlertTypes)
	}
	if len(destination.ExcludeAlertTypes) != 1 || destination.ExcludeAlertTypes[0] != "skyhooks.lost_shields" {
		t.Fatalf("unexpected excluded alert types: %#v", destination.ExcludeAlertTypes)
	}
	if len(destination.ExcludeStructureTypeIDs) != 1 || destination.ExcludeStructureTypeIDs[0] != "81080" {
		t.Fatalf("unexpected excluded structure type IDs: %#v", destination.ExcludeStructureTypeIDs)
	}
}

func TestLoadNormalizesNestedAlertGroups(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, `[{"name":"alerts","webhookUrls":["https://discord.com/api/webhooks/123/token"],"alertTypes":["STRUCTURES.STATE","skyhooks.*"]}]`)

	config, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	got := config.AlertDestinations[0].AlertTypes
	if len(got) != 2 || got[0] != "structures.state" || got[1] != "skyhooks" {
		t.Fatalf("normalized alert types = %#v", got)
	}
}

func TestLoadRejectsInvalidStructureTypeFilter(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, `[{"name":"alerts","webhookUrls":["https://discord.com/api/webhooks/123/token"],"alertTypes":["all"],"excludeStructureTypeIDs":["not-a-type"]}]`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected invalid structure type filter error")
	}
}

func TestLoadAllowsSingleLeafGroupExclusion(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, `[{"name":"alerts","webhookUrls":["https://discord.com/api/webhooks/123/token"],"alertTypes":["all"],"excludeAlertTypes":["structures.state.unanchoring"]}]`)

	config, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if config.AlertDestinations[0].ExcludeAlertTypes[0] != "structures.state.unanchoring" {
		t.Fatalf("unexpected excluded alert types: %#v", config.AlertDestinations[0].ExcludeAlertTypes)
	}
}

func TestLoadRejectsGroupExclusion(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, `[{"name":"alerts","webhookUrls":["https://discord.com/api/webhooks/123/token"],"alertTypes":["all"],"excludeAlertTypes":["skyhooks"]}]`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected group exclusion error")
	}
}

func TestLoadRejectsMixedAllDestinationAlertTypes(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, `[{"name":"alerts","webhookUrls":["https://discord.com/api/webhooks/123/token"],"alertTypes":["all","structures.resources"]}]`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected mixed all shorthand error")
	}
}

func TestLoadRejectsUnknownDestinationAlertType(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, `[{"name":"alerts","webhookUrls":["https://discord.com/api/webhooks/123/token"],"alertTypes":["unknown"]}]`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected unknown alert type error")
	}
}

func TestLoadRejectsEmptyDestinationConfiguration(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, "[]")

	_, err := Load()
	if err == nil {
		t.Fatal("expected empty destination configuration error")
	}
}
