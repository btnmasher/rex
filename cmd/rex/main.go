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
	"github.com/btnmasher/rex/internal/delivery"
	"github.com/btnmasher/rex/internal/discord"
	"github.com/btnmasher/rex/internal/enrichment"
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
	jobWorkerCount             = 3
)

type application struct {
	notificationPoller   *poller.Poller
	notificationDelivery *delivery.Worker
	tokenRefreshJob      *tokenrefresh.Job
	logger               *slog.Logger
}

type notificationAdmission struct {
	delivery.Enqueuer
	poller.PreRouter
}

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

	app, err := newApplication(appContext, &cfg, pool, jobStore, notificationStateStore, logger)
	if err != nil {
		return err
	}
	return app.run(appContext)
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

func newApplication(
	ctx context.Context,
	cfg *config.Config,
	pool *pgxpool.Pool,
	jobStore jobstore.Store,
	notificationStateStore *notificationstatesqlite.Store,
	logger *slog.Logger,
) (*application, error) {
	if ctx == nil || cfg == nil || pool == nil || jobStore == nil || notificationStateStore == nil {
		return nil, errors.New("application dependencies are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	database := authnextdb.New(pool)
	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}
	tokenExportClient, err := tokenexport.NewClient(tokenexport.ClientConfig{
		BaseURL:     cfg.AuthNextTokenExportBaseURL,
		BearerToken: cfg.AuthNextTokenExportBearerToken,
		TokenCount:  cfg.AuthNextTokenExportCount,
		HTTPClient:  httpClient,
	})
	if err != nil {
		return nil, err
	}
	tokens, err := token.NewProvider(tokenExportClient, cfg.PollInterval)
	if err != nil {
		return nil, err
	}
	refreshJob, err := tokenrefresh.New(jobStore, tokenExportClient, tokens, tokenRefreshConcurrency, logger)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	universeResolver := universe.NewResolverWithLogger(database, esiClient, logger)
	discordDelivery := discord.NewWebhookDelivery(httpClient, discord.WithLogger(logger))
	enrichmentService := enrichment.NewService(database, universeResolver, logger)
	destinations, err := alerts.DestinationsFromConfig(cfg.AlertDestinations)
	if err != nil {
		return nil, err
	}
	alertService, err := alerts.NewService(database, discordDelivery, &alerts.Config{
		Destinations: destinations,
		Enricher:     enrichmentService,
		LogPayloads:  cfg.LogPayloads,
		Logger:       logger,
		History:      notificationStateStore,
	})
	if err != nil {
		return nil, err
	}
	discordAdapter, err := alerts.NewDiscordAdapter(alertService)
	if err != nil {
		return nil, err
	}
	notificationDelivery, err := delivery.New(&delivery.Config{
		Store:          notificationStateStore,
		Enricher:       enrichmentService,
		Router:         alertService,
		Adapter:        discordAdapter,
		TerminalRecord: alertService,
		Concurrency:    notificationConcurrency,
		Logger:         logger,
	})
	if err != nil {
		return nil, err
	}
	notificationPoller, err := poller.NewWithConfig(database, tokens, esiClient, notificationAdmission{
		Enqueuer:  notificationDelivery,
		PreRouter: alertService,
	}, poller.Config{
		Interval:    cfg.PollInterval,
		Lookbehind:  cfg.PollLookbehind,
		Concurrency: notificationConcurrency,
		Logger:      logger,
		StateStore:  notificationStateStore,
	})
	if err != nil {
		return nil, err
	}
	return &application{
		notificationPoller:   notificationPoller,
		notificationDelivery: notificationDelivery,
		tokenRefreshJob:      refreshJob,
		logger:               logger,
	}, nil
}

func (a *application) run(ctx context.Context) error {
	if a == nil || a.notificationPoller == nil || a.notificationDelivery == nil || a.tokenRefreshJob == nil {
		return errors.New("application workers are required")
	}
	if ctx == nil {
		return errors.New("application context is required")
	}
	logger := a.logger
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
			case <-a.tokenRefreshJob.Ready():
				logger.Info("access tokens available; starting notification poller")
				return a.notificationPoller.Run(jobContext)
			case <-jobContext.Done():
				return jobContext.Err()
			}
		})
	}()
	go func() {
		defer workers.Done()
		workerErrors <- runWorkerSafely(logger, "notification delivery worker", func() error {
			return a.notificationDelivery.Run(jobContext)
		})
	}()
	go func() {
		defer workers.Done()
		workerErrors <- runWorkerSafely(logger, "access-token refresh scheduler", func() error {
			return a.tokenRefreshJob.RunPeriodically(jobContext)
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
