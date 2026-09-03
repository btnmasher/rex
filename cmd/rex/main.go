package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/btnmasher/rex/jobruntime/jobstore"
	jobmemory "github.com/btnmasher/rex/jobruntime/jobstore/memory"
	jobpostgres "github.com/btnmasher/rex/jobruntime/jobstore/postgres"
	jobsqlite "github.com/btnmasher/rex/jobruntime/jobstore/sqlite"

	"github.com/btnmasher/rex/internal/alerts"
	"github.com/btnmasher/rex/internal/authnextdb"
	"github.com/btnmasher/rex/internal/config"
	"github.com/btnmasher/rex/internal/discord"
	"github.com/btnmasher/rex/internal/esi"
	"github.com/btnmasher/rex/internal/logging"
	notificationstatesqlite "github.com/btnmasher/rex/internal/notificationstate/sqlite"
	"github.com/btnmasher/rex/internal/poller"
	"github.com/btnmasher/rex/internal/token"
	"github.com/btnmasher/rex/internal/tokenexport"
	"github.com/btnmasher/rex/internal/tokenrefresh"
	"github.com/btnmasher/rex/internal/universe"
)

const (
	notificationConcurrency    = 8
	tokenRefreshConcurrency    = 8
	tokenRefreshStartupTimeout = 2 * time.Minute
	jobWorkerCount             = 2
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--migrate":
			commandContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			if err := runMigrations(context.WithoutCancel(commandContext), os.Args[2:]); err != nil && !errors.Is(err, context.Canceled) {
				stop()
				slog.Error("database migration failed", "err", err)
				os.Exit(1)
			}
			stop()
			return
		case "--discord-debug":
			commandContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			if err := runDiscordDebug(commandContext); err != nil {
				stop()
				slog.Error("synthetic Discord alert delivery failed", "err", err)
				os.Exit(1)
			}
			stop()
			return
		}
	}
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("notification service failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	appContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := logging.New(cfg.LogLevel, cfg.LogPretty)
	slog.SetDefault(logger)
	logger.Info(
		"configuration loaded",
		"poll_interval", cfg.PollInterval,
		"poll_lookbehind", cfg.PollLookbehind,
		"http_timeout", cfg.HTTPTimeout,
		"job_store", cfg.JobStore,
		"log_level", cfg.LogLevel,
	)

	startupContext := context.WithoutCancel(appContext)
	pool, err := newDatabase(startupContext, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	jobStore, closeJobStore, err := configureJobStore(startupContext, cfg.JobStore, cfg.JobStoreSQLitePath, pool)
	if err != nil {
		return err
	}
	defer closeJobStore()
	logger.Info("job store configured", "kind", cfg.JobStore, "implementation", fmt.Sprintf("%T", jobStore))
	notificationStateStore, err := notificationstatesqlite.NewStore(startupContext, cfg.NotificationStateSQLitePath)
	if err != nil {
		return err
	}
	defer func() { _ = notificationStateStore.Close() }()
	logger.Info("notification state store configured", "path", cfg.NotificationStateSQLitePath)

	notificationPoller, tokenRefreshJob, err := newNotificationPoller(appContext, &cfg, pool, jobStore, notificationStateStore, logger)
	if err != nil {
		return err
	}
	return runJobs(appContext, notificationPoller, tokenRefreshJob, logger)
}

func newDatabase(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	if ctx == nil {
		return nil, errors.New("database context is required")
	}
	databaseConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database configuration: %w", err)
	}
	databaseConfig.MaxConns = 8
	databaseConfig.MinConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, databaseConfig)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	return pool, nil
}

func newNotificationPoller(
	ctx context.Context,
	cfg *config.Config,
	pool *pgxpool.Pool,
	jobStore jobstore.Store,
	notificationStateStore *notificationstatesqlite.Store,
	logger *slog.Logger,
) (*poller.Poller, *tokenrefresh.Job, error) {
	database := authnextdb.New(pool)
	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}
	tokenExportClient, err := tokenexport.NewClient(cfg.AuthNextTokenExportBaseURL, cfg.AuthNextTokenExportBearerToken, httpClient)
	if err != nil {
		return nil, nil, err
	}
	tokens, err := token.NewProvider(tokenExportClient)
	if err != nil {
		return nil, nil, err
	}
	refreshJob, err := tokenrefresh.New(jobStore, tokenExportClient, tokens, tokenRefreshConcurrency, logger)
	if err != nil {
		return nil, nil, err
	}
	startupContext, cancelStartup := context.WithTimeout(ctx, tokenRefreshStartupTimeout)
	refreshStartedAt := time.Now()
	runID, err := refreshJob.Run(startupContext)
	cancelStartup()
	if err != nil {
		logger.Warn(
			"initial access-token refresh job failed; notification polling is disabled until a refresh succeeds",
			"run_id", runID,
			"duration", time.Since(refreshStartedAt),
			"err", err,
		)
	} else {
		logger.Info(
			"initial access-token refresh job finished",
			"run_id", runID,
			"duration", time.Since(refreshStartedAt),
			"usable_tokens", tokens.HasUsableTokens(),
		)
	}
	esiClient, err := esi.NewClient(
		cfg.ESIBaseURL,
		cfg.CompatibilityDate,
		httpClient,
		esi.WithClientID(cfg.EVEClientID),
		esi.WithLogger(logger),
	)
	if err != nil {
		return nil, nil, err
	}
	universeResolver := universe.NewResolverWithLogger(database, esiClient, logger)
	delivery := discord.NewWebhookDelivery(httpClient, discord.WithLogger(logger))
	destinations := make([]alerts.Destination, 0, len(cfg.AlertDestinations))
	for i := range cfg.AlertDestinations {
		destination := &cfg.AlertDestinations[i]
		destinations = append(destinations, alerts.Destination{
			ID:                      destination.Name,
			WebhookURLs:             destination.WebhookURLs,
			AlertTypes:              destination.AlertTypes,
			ExcludeAlertTypes:       destination.ExcludeAlertTypes,
			ExcludeStructureTypeIDs: destination.ExcludeStructureTypeIDs,
		})
	}
	alertService, err := alerts.NewService(database, delivery, &alerts.Config{
		Destinations:            destinations,
		OverrideSenderName:      cfg.DiscordOverrideSenderName,
		OverrideSenderAvatarURL: cfg.DiscordOverrideSenderAvatarURL,
		ShowEntityIDs:           cfg.DiscordShowEntityIDs,
		LogPayloads:             cfg.LogPayloads,
		Logger:                  logger,
		UniverseResolver:        universeResolver,
		History:                 notificationStateStore,
	})
	if err != nil {
		return nil, nil, err
	}
	notificationPoller, err := poller.NewWithConfig(database, tokens, esiClient, alertService, poller.Config{
		Interval:    cfg.PollInterval,
		Lookbehind:  cfg.PollLookbehind,
		Concurrency: notificationConcurrency,
		Logger:      logger,
		StateStore:  notificationStateStore,
	})
	if err != nil {
		return nil, nil, err
	}
	return notificationPoller, refreshJob, nil
}

func runJobs(
	ctx context.Context,
	notificationPoller *poller.Poller,
	tokenRefreshJob *tokenrefresh.Job,
	logger *slog.Logger,
) error {
	if notificationPoller == nil || tokenRefreshJob == nil {
		return errors.New("job workers are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	jobContext, cancel := context.WithCancel(ctx)
	defer cancel()

	workerErrors := make(chan error, jobWorkerCount)
	var workers sync.WaitGroup
	workers.Add(jobWorkerCount)
	go func() {
		defer workers.Done()
		workerErrors <- runWorkerSafely(logger, "notification poller", func() error {
			select {
			case <-tokenRefreshJob.Ready():
				logger.Info("access tokens available; starting notification poller")
				return notificationPoller.Run(jobContext)
			case <-jobContext.Done():
				return jobContext.Err()
			}
		})
	}()
	go func() {
		defer workers.Done()
		workerErrors <- runWorkerSafely(logger, "access-token refresh scheduler", func() error {
			return tokenRefreshJob.RunPeriodically(jobContext)
		})
	}()

	select {
	case <-ctx.Done():
		cancel()
		workers.Wait()
		return nil
	case workerErr := <-workerErrors:
		cancel()
		workers.Wait()
		if errors.Is(workerErr, context.Canceled) {
			return nil
		}
		return workerErr
	}
}

func runWorkerSafely(logger *slog.Logger, name string, worker func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Error("job worker panic recovered",
				"worker", name,
				"panic_type", fmt.Sprintf("%T", recovered),
				"panic", recovered,
				"stack", string(debug.Stack()),
			)
			err = fmt.Errorf("%s panic: %v", name, recovered)
		}
	}()
	return worker()
}

func configureJobStore(ctx context.Context, kind, sqlitePath string, pool *pgxpool.Pool) (jobstore.Store, func(), error) {
	if ctx == nil {
		return nil, nil, errors.New("job store context is required")
	}
	switch kind {
	case "memory":
		return jobmemory.NewStore(), func() {}, nil
	case "sqlite":
		store, err := jobsqlite.NewStore(ctx, sqlitePath)
		if err != nil {
			return nil, nil, err
		}
		return store, func() { _ = store.Close() }, nil
	case "postgres":
		store, err := jobpostgres.NewStoreWithPool(ctx, pool)
		if err != nil {
			return nil, nil, err
		}
		return store, func() {}, nil
	default:
		return nil, nil, fmt.Errorf("unsupported job store %q", kind)
	}
}
