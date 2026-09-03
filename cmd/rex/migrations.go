package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/btnmasher/rex/jobruntime/jobstore/postgres"
	jobsqlite "github.com/btnmasher/rex/jobruntime/jobstore/sqlite"
	"github.com/jackc/pgx/v5/pgxpool"
)

func runMigrations(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("migration context is required")
	}
	flags := flag.NewFlagSet("rex", flag.ContinueOnError)
	storeKind := flags.String("store", "sqlite", "job-store backend to migrate (sqlite or postgres)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	switch strings.TrimSpace(*storeKind) {
	case "sqlite":
		return migrateSQLite(ctx)
	case "postgres":
		return migratePostgres(ctx)
	default:
		return fmt.Errorf("unsupported migration store %q", *storeKind)
	}
}

func migrateSQLite(ctx context.Context) error {
	path := strings.TrimSpace(os.Getenv("JOB_STORE_SQLITE_PATH"))
	if path == "" {
		path = "rex-jobruntime.sqlite"
	}
	store, err := jobsqlite.NewStore(ctx, path)
	if err != nil {
		return err
	}
	if err := store.Close(); err != nil {
		return fmt.Errorf("close SQLite migration store: %w", err)
	}
	return nil
}

func migratePostgres(ctx context.Context) error {
	if strings.TrimSpace(os.Getenv("JOB_STORE")) != "postgres" {
		return errors.New("JOB_STORE must be postgres to apply PostgreSQL job-store migrations")
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("parse database configuration: %w", err)
	}
	cfg.MaxConns = 1
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("create database pool: %w", err)
	}
	defer pool.Close()

	return postgres.Migrate(ctx, pool)
}
