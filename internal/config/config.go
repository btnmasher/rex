// Package config parses and validates the service environment.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/btnmasher/rex/internal/discord"
	"github.com/btnmasher/rex/internal/notifications"
)

const (
	allAlertCategories       = "all"
	allAlertWildcard         = "*"
	defaultESIBaseURL        = "https://esi.evetech.net"
	defaultCompatibilityDate = "2026-05-19"
	defaultPollInterval      = time.Minute
	defaultPollLookbehind    = 10 * time.Minute
	maxPollLookbehind        = 10 * time.Minute
	defaultHTTPTimeout       = 20 * time.Second
	defaultJobStore          = "memory"
	defaultSQLitePath        = "rex-jobruntime.sqlite"
	defaultNotificationState = "rex-notification-state.sqlite"
	defaultLogLevel          = "INFO"
	defaultAlertDestinations = "alert-destinations.json"
	maxSenderNameCharacters  = 80
	maxStructureTypeFilters  = 128
)

// Config contains validated runtime settings.
type Config struct {
	DatabaseURL                    string
	EVEClientID                    string
	AuthNextTokenExportBaseURL     string
	AuthNextTokenExportBearerToken string
	ESIBaseURL                     string
	CompatibilityDate              string
	PollInterval                   time.Duration
	PollLookbehind                 time.Duration
	HTTPTimeout                    time.Duration
	JobStore                       string
	JobStoreSQLitePath             string
	NotificationStateSQLitePath    string
	AlertDestinationsFile          string
	AlertDestinations              []AlertDestination
	DiscordOverrideSenderName      string
	DiscordOverrideSenderAvatarURL string
	DiscordShowEntityIDs           bool
	LogLevel                       string
	LogPretty                      bool
	LogPayloads                    bool
}

// AlertConfig contains the validated configuration needed to deliver alerts.
// It intentionally excludes database and token-export settings so local tools
// can exercise Discord delivery without connecting to service dependencies.
type AlertConfig struct {
	AlertDestinationsFile          string
	AlertDestinations              []AlertDestination
	DiscordOverrideSenderName      string
	DiscordOverrideSenderAvatarURL string
	DiscordShowEntityIDs           bool
	LogPretty                      bool
	LogPayloads                    bool
}

// AlertDestination is a named Discord destination and its canonical alert selector mapping.
type AlertDestination struct {
	Name                    string   `json:"name"`
	WebhookURLs             []string `json:"webhookUrls,omitempty"`
	AlertTypes              []string `json:"alertTypes"`
	ExcludeAlertTypes       []string `json:"excludeAlertTypes,omitempty"`
	ExcludeStructureTypeIDs []string `json:"excludeStructureTypeIDs,omitempty"`
}

// Load reads and validates configuration from environment variables.
func Load() (Config, error) {
	databaseURL, err := required("DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	authNextBaseURL, err := required("AUTH_NEXT_TOKEN_EXPORT_BASE_URL")
	if err != nil {
		return Config{}, err
	}
	authNextBearerToken, err := required("AUTH_NEXT_TOKEN_EXPORT_BEARER_TOKEN")
	if err != nil {
		return Config{}, err
	}
	alertConfig, err := LoadAlertConfig()
	if err != nil {
		return Config{}, err
	}
	clientID, err := required("EVE_SSO_CLIENT_ID")
	if err != nil {
		return Config{}, err
	}
	config := Config{
		DatabaseURL:                    databaseURL,
		EVEClientID:                    clientID,
		AuthNextTokenExportBaseURL:     authNextBaseURL,
		AuthNextTokenExportBearerToken: authNextBearerToken,
		ESIBaseURL:                     valueOrDefault("ESI_BASE_URL", defaultESIBaseURL),
		CompatibilityDate:              valueOrDefault("ESI_COMPATIBILITY_DATE", defaultCompatibilityDate),
		PollInterval:                   defaultPollInterval,
		PollLookbehind:                 defaultPollLookbehind,
		HTTPTimeout:                    defaultHTTPTimeout,
		JobStore:                       valueOrDefault("JOB_STORE", defaultJobStore),
		JobStoreSQLitePath:             valueOrDefault("JOB_STORE_SQLITE_PATH", defaultSQLitePath),
		NotificationStateSQLitePath:    valueOrDefault("NOTIFICATION_STATE_SQLITE_PATH", defaultNotificationState),
		AlertDestinationsFile:          alertConfig.AlertDestinationsFile,
		AlertDestinations:              alertConfig.AlertDestinations,
		DiscordOverrideSenderName:      alertConfig.DiscordOverrideSenderName,
		DiscordOverrideSenderAvatarURL: alertConfig.DiscordOverrideSenderAvatarURL,
		DiscordShowEntityIDs:           alertConfig.DiscordShowEntityIDs,
		LogPretty:                      alertConfig.LogPretty,
		LogPayloads:                    alertConfig.LogPayloads,
		LogLevel:                       strings.ToUpper(valueOrDefault("LOG_LEVEL", defaultLogLevel)),
	}
	config.PollInterval, err = durationValue("POLL_INTERVAL", config.PollInterval)
	if err != nil {
		return Config{}, err
	}
	config.PollLookbehind, err = durationValue("POLL_LOOKBEHIND", config.PollLookbehind)
	if err != nil {
		return Config{}, err
	}
	if config.PollLookbehind > maxPollLookbehind {
		config.PollLookbehind = maxPollLookbehind
	}
	config.HTTPTimeout, err = durationValue("HTTP_TIMEOUT", config.HTTPTimeout)
	if err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(config.NotificationStateSQLitePath) == "" {
		return Config{}, errors.New("NOTIFICATION_STATE_SQLITE_PATH must not be empty")
	}
	if err := validateJobStore(config.JobStore, config.JobStoreSQLitePath); err != nil {
		return Config{}, err
	}
	return config, nil
}

// LoadAlertConfig reads and validates the local Discord alert configuration.
func LoadAlertConfig() (AlertConfig, error) {
	showEntityIDs, err := boolValue("DISCORD_SHOW_ENTITY_IDS", false)
	if err != nil {
		return AlertConfig{}, err
	}
	logPretty, err := boolValue("LOG_PRETTY", false)
	if err != nil {
		return AlertConfig{}, err
	}
	logPayloads, err := boolValue("LOG_PAYLOADS", false)
	if err != nil {
		return AlertConfig{}, err
	}
	config := AlertConfig{
		AlertDestinationsFile:          valueOrDefault("ALERT_DESTINATIONS_FILE", defaultAlertDestinations),
		DiscordOverrideSenderName:      valueOrDefault("DISCORD_OVERRIDE_SENDER_NAME", "Rex Alerts"),
		DiscordOverrideSenderAvatarURL: strings.TrimSpace(os.Getenv("DISCORD_OVERRIDE_SENDER_AVATAR_URL")),
		DiscordShowEntityIDs:           showEntityIDs,
		LogPretty:                      logPretty,
		LogPayloads:                    logPayloads,
	}
	config.AlertDestinations, err = parseAlertDestinations(config.AlertDestinationsFile)
	if err != nil {
		return AlertConfig{}, err
	}
	if err := validateSenderOverride(config.DiscordOverrideSenderName, config.DiscordOverrideSenderAvatarURL); err != nil {
		return AlertConfig{}, err
	}
	return config, nil
}

func durationValue(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration: %q", name, raw)
	}
	return parsed, nil
}

func boolValue(name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %q", name, raw)
	}
	return parsed, nil
}

func parseAlertDestinations(path string) ([]AlertDestination, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("ALERT_DESTINATIONS_FILE is required")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // path is an operator-configured local file
	if err != nil {
		return nil, fmt.Errorf("read ALERT_DESTINATIONS_FILE %q: %w", path, err)
	}
	var destinations []AlertDestination
	if err := json.Unmarshal(raw, &destinations); err != nil {
		return nil, fmt.Errorf("ALERT_DESTINATIONS_FILE %q must contain valid JSON: %w", path, err)
	}
	seenNames := make(map[string]struct{}, len(destinations))
	for i := range destinations {
		destination := &destinations[i]
		if err := validateAlertDestination(path, i, destination, seenNames); err != nil {
			return nil, err
		}
	}
	if len(destinations) == 0 {
		return nil, fmt.Errorf("ALERT_DESTINATIONS_FILE %q must contain at least one destination", path)
	}
	return destinations, nil
}

func validateAlertDestination(path string, index int, destination *AlertDestination, seenNames map[string]struct{}) error {
	destination.Name = strings.TrimSpace(destination.Name)
	for i := range destination.WebhookURLs {
		destination.WebhookURLs[i] = strings.TrimSpace(destination.WebhookURLs[i])
	}
	if destination.Name == "" {
		return fmt.Errorf("ALERT_DESTINATIONS_FILE %q destination %d has no name", path, index)
	}
	if _, ok := seenNames[destination.Name]; ok {
		return fmt.Errorf("ALERT_DESTINATIONS_FILE %q contains duplicate destination name %q", path, destination.Name)
	}
	seenNames[destination.Name] = struct{}{}
	if len(destination.WebhookURLs) == 0 {
		return fmt.Errorf("ALERT_DESTINATIONS_FILE %q destination %q has no webhook URLs", path, destination.Name)
	}
	for i, webhookURL := range destination.WebhookURLs {
		if err := discord.ValidateWebhookURL(webhookURL); err != nil {
			return fmt.Errorf("ALERT_DESTINATIONS_FILE %q destination %q webhook %d: %w", path, destination.Name, i+1, err)
		}
	}
	if err := validateAlertTypes(destination.Name, destination.AlertTypes, destination.ExcludeAlertTypes); err != nil {
		return fmt.Errorf("ALERT_DESTINATIONS_FILE %q: %w", path, err)
	}
	if err := validateStructureTypeIDs(destination.Name, destination.ExcludeStructureTypeIDs); err != nil {
		return fmt.Errorf("ALERT_DESTINATIONS_FILE %q: %w", path, err)
	}
	return nil
}

func validateAlertTypes(destinationName string, values, exclusions []string) error {
	if len(values) == 0 {
		return fmt.Errorf("destination %q has no alert types", destinationName)
	}
	if err := validateIncludedAlertTypes(destinationName, values); err != nil {
		return err
	}
	return validateExcludedAlertTypes(destinationName, exclusions)
}

func validateIncludedAlertTypes(destinationName string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for i := range values {
		value := strings.ToLower(strings.TrimSpace(values[i]))
		if value == "" {
			return fmt.Errorf("destination %q contains an empty alert type", destinationName)
		}
		if value == allAlertCategories || value == allAlertWildcard {
			if len(values) != 1 {
				return fmt.Errorf("destination %q cannot combine %q with other alert types", destinationName, value)
			}
			values[i] = value
			continue
		}
		canonical, err := notifications.NormalizeAlertSelector(value)
		if err != nil {
			return fmt.Errorf("destination %q has invalid alert type %q: %w", destinationName, value, err)
		}
		if _, ok := seen[canonical]; ok {
			return fmt.Errorf("destination %q contains duplicate alert type %q", destinationName, canonical)
		}
		seen[canonical] = struct{}{}
		values[i] = canonical
	}
	return nil
}

func validateExcludedAlertTypes(destinationName string, exclusions []string) error {
	seen := make(map[string]struct{}, len(exclusions))
	for i := range exclusions {
		value := strings.ToLower(strings.TrimSpace(exclusions[i]))
		if value == "" {
			return fmt.Errorf("destination %q contains an empty excluded alert type", destinationName)
		}
		if value == allAlertCategories || value == allAlertWildcard {
			return fmt.Errorf("destination %q can exclude only individual alert types, got %q", destinationName, value)
		}
		canonical, err := notifications.NormalizeAlertSelector(value)
		if err != nil {
			return fmt.Errorf("destination %q has invalid excluded alert type %q: %w", destinationName, value, err)
		}
		if notifications.IsAlertGroup(canonical) {
			return fmt.Errorf("destination %q can exclude only individual alert types, got %q", destinationName, value)
		}
		if _, ok := seen[canonical]; ok {
			return fmt.Errorf("destination %q contains duplicate excluded alert type %q", destinationName, canonical)
		}
		seen[canonical] = struct{}{}
		exclusions[i] = canonical
	}
	return nil
}

func validateStructureTypeIDs(destinationName string, values []string) error {
	if len(values) > maxStructureTypeFilters {
		return fmt.Errorf("destination %q has more than %d excluded structure type IDs", destinationName, maxStructureTypeFilters)
	}
	seen := make(map[string]struct{}, len(values))
	for i := range values {
		value := strings.TrimSpace(values[i])
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil || parsed == 0 {
			return fmt.Errorf("destination %q has invalid excluded structure type ID %q", destinationName, value)
		}
		value = strconv.FormatUint(parsed, 10)
		if _, ok := seen[value]; ok {
			return fmt.Errorf("destination %q contains duplicate excluded structure type ID %q", destinationName, value)
		}
		seen[value] = struct{}{}
		values[i] = value
	}
	return nil
}

func validateSenderOverride(name, avatarURL string) error {
	if utf8.RuneCountInString(name) > maxSenderNameCharacters {
		return fmt.Errorf("DISCORD_OVERRIDE_SENDER_NAME must not exceed %d characters", maxSenderNameCharacters)
	}
	if avatarURL == "" {
		return nil
	}
	parsed, err := url.Parse(avatarURL)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return errors.New("DISCORD_OVERRIDE_SENDER_AVATAR_URL must be an absolute HTTP or HTTPS URL")
	}
	return nil
}

func validateJobStore(kind, sqlitePath string) error {
	switch kind {
	case "memory", "postgres":
		return nil
	case "sqlite":
		if strings.TrimSpace(sqlitePath) == "" {
			return errors.New("JOB_STORE_SQLITE_PATH must not be empty when JOB_STORE=sqlite")
		}
		return nil
	default:
		return fmt.Errorf("JOB_STORE must be memory, sqlite, or postgres, got %q", kind)
	}
}

func required(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func valueOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
