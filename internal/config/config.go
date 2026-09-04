// Package config parses and validates the service environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/btnmasher/rex/internal/tokenexport"
)

const (
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
	defaultTokenExportCount  = 60
)

// Config contains validated runtime settings.
type Config struct {
	DatabaseURL                    string
	EVEClientID                    string
	AuthNextTokenExportBaseURL     string
	AuthNextTokenExportBearerToken string
	AuthNextTokenExportCount       int
	ESIBaseURL                     string
	CompatibilityDate              string
	PollInterval                   time.Duration
	PollLookbehind                 time.Duration
	HTTPTimeout                    time.Duration
	JobStore                       string
	JobStoreSQLitePath             string
	NotificationStateSQLitePath    string
	AlertDestinationsConfig        string
	AlertDestinations              []AlertDestination
	LogLevel                       string
	LogPretty                      bool
	LogPayloads                    bool
}

// AlertConfig contains the validated configuration needed to deliver alerts.
// It intentionally excludes database and token-export settings so local tools
// can exercise Discord delivery without connecting to service dependencies.
type AlertConfig struct {
	AlertDestinationsConfig string
	AlertDestinations       []AlertDestination
	LogPretty               bool
	LogPayloads             bool
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
	authNextTokenCount, err := positiveIntValue("AUTH_NEXT_TOKEN_EXPORT_COUNT", defaultTokenExportCount, tokenexport.MaxTokensPerCorporation)
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
		AuthNextTokenExportCount:       authNextTokenCount,
		ESIBaseURL:                     valueOrDefault("ESI_BASE_URL", defaultESIBaseURL),
		CompatibilityDate:              valueOrDefault("ESI_COMPATIBILITY_DATE", defaultCompatibilityDate),
		PollInterval:                   defaultPollInterval,
		PollLookbehind:                 defaultPollLookbehind,
		HTTPTimeout:                    defaultHTTPTimeout,
		JobStore:                       valueOrDefault("JOB_STORE", defaultJobStore),
		JobStoreSQLitePath:             valueOrDefault("JOB_STORE_SQLITE_PATH", defaultSQLitePath),
		NotificationStateSQLitePath:    valueOrDefault("NOTIFICATION_STATE_SQLITE_PATH", defaultNotificationState),
		AlertDestinationsConfig:        alertConfig.AlertDestinationsConfig,
		AlertDestinations:              alertConfig.AlertDestinations,
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
	logPretty, err := boolValue("LOG_PRETTY", false)
	if err != nil {
		return AlertConfig{}, err
	}
	logPayloads, err := boolValue("LOG_PAYLOADS", false)
	if err != nil {
		return AlertConfig{}, err
	}
	config := AlertConfig{
		AlertDestinationsConfig: valueOrDefault("ALERT_DESTINATIONS_CONFIG", defaultAlertDestinations),
		LogPretty:               logPretty,
		LogPayloads:             logPayloads,
	}
	config.AlertDestinations, err = parseAlertDestinations(config.AlertDestinationsConfig)
	if err != nil {
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

func positiveIntValue(name string, fallback, maximum int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 1 || parsed > maximum {
		return 0, fmt.Errorf("%s must be an integer between 1 and %d: %q", name, maximum, raw)
	}
	return parsed, nil
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
