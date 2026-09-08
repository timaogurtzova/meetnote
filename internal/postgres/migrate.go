package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const migrationLockID int64 = 681946213

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		return err
	}
	defer closeProvider()

	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply database migrations: %w", err)
	}
	return nil
}

func newMigrationProvider(pool *pgxpool.Pool) (*goose.Provider, func(), error) {
	if pool == nil {
		return nil, nil, errors.New("database pool must not be nil")
	}
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return nil, nil, fmt.Errorf("open embedded migrations: %w", err)
	}
	sessionLocker, err := lock.NewPostgresSessionLocker(
		lock.WithLockID(migrationLockID),
		lock.WithLockTimeout(1, 30),
		lock.WithUnlockTimeout(1, 5),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("configure migration lock: %w", err)
	}
	database := stdlib.OpenDBFromPool(pool)
	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		database,
		migrations,
		goose.WithSessionLocker(sessionLocker),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		_ = database.Close()
		return nil, nil, fmt.Errorf("create migration provider: %w", err)
	}
	return provider, func() {
		_ = database.Close()
	}, nil
}
