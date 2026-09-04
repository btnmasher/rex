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
	t.Setenv("AUTH_NEXT_TOKEN_EXPORT_COUNT", "60")
	t.Setenv("EVE_SSO_CLIENT_ID", "test-client")
	writeDestinationsFile(t, "alert-destinations.json", `{"version":1,"destinations":[{"name":"test-alerts","filters":{"alertTypes":["all"]},"delivery":{"type":"discord","targets":[{"id":"primary","webhookUrl":"https://discord.com/api/webhooks/123/token"}]}}]}`)
}

func writeDestinationsFile(t *testing.T, name, contents string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write destinations file: %v", err)
	}
	t.Setenv("ALERT_DESTINATIONS_CONFIG", path)
}

func TestLoadTokenExportConfiguration(t *testing.T) {
	setRequiredEnvironment(t)

	loaded, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if loaded.AuthNextTokenExportBaseURL != "http://localhost:3000" || loaded.AuthNextTokenExportBearerToken != "test-bearer" || loaded.AuthNextTokenExportCount != 60 {
		t.Fatalf("unexpected token export configuration: %#v", loaded)
	}
}

func TestLoadRejectsInvalidTokenExportCount(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("AUTH_NEXT_TOKEN_EXPORT_COUNT", "65")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid token export count to fail")
	}
}

func TestLoadAlertConfiguration(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, "alert-destinations.yaml", `
version: 1
destinations:
  # Comments are supported in YAML configuration.
  - name: structure-alerts
    filters:
      alertTypes: [structures.state]
      excludeAlertTypes: [structures.state.vulnerable]
      includeCorporationIDs: ["000123456789"]
    presentation:
      showEntityIDs: true
    delivery:
      type: discord
      targets:
        - id: primary
          webhookUrl: https://discord.com/api/webhooks/123/token
      mentionRules:
        - alertTypes: [structures.state]
          mention: here
`)
	loaded, err := LoadAlertConfig()
	if err != nil {
		t.Fatalf("load alert config: %v", err)
	}
	if loaded.AlertDestinationsConfig == "" || len(loaded.AlertDestinations) != 1 {
		t.Fatalf("unexpected alert config: %#v", loaded)
	}
	destination := loaded.AlertDestinations[0]
	if destination.Name != "structure-alerts" || !destination.Presentation.ShowEntityIDs || destination.Delivery.Type != "discord" {
		t.Fatalf("unexpected destination: %#v", destination)
	}
	if len(destination.Filters.AlertTypes) != 1 || destination.Filters.AlertTypes[0] != "structures.state" || destination.Filters.ExcludeAlertTypes[0] != "structures.state.vulnerable" {
		t.Fatalf("unexpected filters: %#v", destination.Filters)
	}
}

func TestLoadRejectsUnsupportedConfigurationVersion(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, "alert-destinations.json", `{"version":2,"destinations":[]}`)
	if _, err := LoadAlertConfig(); err == nil {
		t.Fatal("expected unsupported version error")
	}
}

func TestLoadRejectsMissingConfigurationVersion(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, "alert-destinations.json", `{"destinations":[]}`)
	if _, err := LoadAlertConfig(); err == nil {
		t.Fatal("expected missing version error")
	}
}

func TestLoadRejectsUnknownConfigurationFields(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, "alert-destinations.json", `{"version":1,"destinations":[{"name":"alerts","filters":{"alertTypes":["all"]},"delivery":{"type":"discord","targets":[]},"legacy":true}]}`)
	if _, err := LoadAlertConfig(); err == nil {
		t.Fatal("expected unknown field error")
	}
}

func TestLoadRejectsEmptyDestinationFilters(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, "alert-destinations.json", `{"version":1,"destinations":[{"name":"alerts","filters":{},"delivery":{"type":"discord","targets":[]}}]}`)
	if _, err := LoadAlertConfig(); err == nil {
		t.Fatal("expected empty filter error")
	}
}

func TestLoadRejectsDuplicateDestinationNames(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, "alert-destinations.json", `{"version":1,"destinations":[{"name":"alerts","filters":{"alertTypes":["all"]},"delivery":{"type":"discord","targets":[]}}, {"name":"alerts","filters":{"alertTypes":["all"]},"delivery":{"type":"discord","targets":[]}}]}`)
	if _, err := LoadAlertConfig(); err == nil {
		t.Fatal("expected duplicate destination name error")
	}
}

func TestLoadRejectsInvalidFilters(t *testing.T) {
	setRequiredEnvironment(t)
	writeDestinationsFile(t, "alert-destinations.json", `{"version":1,"destinations":[{"name":"alerts","filters":{"alertTypes":["all","structures"]},"delivery":{"type":"discord","targets":[]}}]}`)
	if _, err := LoadAlertConfig(); err == nil {
		t.Fatal("expected invalid filter error")
	}
}

func TestLoadIgnoresRemovedEnvironmentVariables(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("ALERT_DESTINATIONS_FILE", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("DISCORD_SHOW_ENTITY_IDS", "invalid")
	if _, err := LoadAlertConfig(); err != nil {
		t.Fatalf("removed environment variables should be ignored: %v", err)
	}
}

func TestLoadCapsPollLookbehind(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("POLL_LOOKBEHIND", "1h")
	loaded, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if loaded.PollLookbehind != maxPollLookbehind {
		t.Fatalf("poll lookbehind = %s, want %s", loaded.PollLookbehind, maxPollLookbehind)
	}
}
