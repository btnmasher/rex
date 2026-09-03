package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
)

const gooseTableName = "goose_db_version"

var (
	//go:embed migrations/*.sql
	migrations embed.FS
)

func applyMigrations(ctx context.Context, db *sql.DB) error {
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}
	store, err := database.NewStore(goose.DialectSQLite3, gooseTableName)
	if err != nil {
		return fmt.Errorf("create migration store: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectCustom, db, fsys, goose.WithStore(&migrationStore{Store: store}))
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	if err := validateGooseVersions(ctx, db, provider); err != nil {
		return err
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

type migrationStore struct {
	database.Store
}

func (s *migrationStore) TableExists(ctx context.Context, db database.DBTxConn) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, "select exists(select 1 from sqlite_master where type = 'table' and name = 'goose_db_version')").Scan(&exists); err != nil {
		return false, fmt.Errorf("check Goose migration table: %w", err)
	}
	return exists, nil
}

func validateGooseVersions(ctx context.Context, db *sql.DB, provider *goose.Provider) error {
	var exists bool
	if err := db.QueryRowContext(ctx, "select exists(select 1 from sqlite_master where type = 'table' and name = 'goose_db_version')").Scan(&exists); err != nil {
		return fmt.Errorf("check Goose migration table: %w", err)
	}
	if !exists {
		return nil
	}
	known := map[int64]struct{}{0: {}}
	for _, source := range provider.ListSources() {
		known[source.Version] = struct{}{}
	}
	rows, err := db.QueryContext(ctx, "select distinct version_id from goose_db_version")
	if err != nil {
		return fmt.Errorf("read Goose migration versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return fmt.Errorf("scan Goose migration version: %w", err)
		}
		if _, ok := known[version]; !ok {
			return fmt.Errorf("database contains unknown migration version %d", version)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read Goose migration versions: %w", err)
	}
	return nil
}
