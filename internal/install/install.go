// Package install gives the upgrade of each major version access to the Pocket ID installation it upgrades
package install

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	sqlitekit "github.com/italypaleale/go-sql-utils/sqlite"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/pocket-id/upgrade/internal/storage"
)

// Installation is the Pocket ID installation being upgraded, connected to the way Pocket ID connects to it
type Installation struct {
	Config *Config
	DB     *sql.DB
	// Pool is only set for Postgres, where DB wraps it
	Pool *pgxpool.Pool
}

// Open connects to the database of the installation
func Open(ctx context.Context, cfg *Config) (*Installation, error) {
	if !cfg.Postgres {
		connector, err := sqlitekit.NewConnector(sqlitekit.ConnectOpts{
			ConnString: addSqliteDatetimeParams(cfg.DBConnectionString),
			Logger:     slog.Default(),
		})
		if err != nil {
			return nil, err
		}
		return &Installation{Config: cfg, DB: connector.OpenDB()}, nil
	}

	pool, err := pgxpool.New(ctx, cfg.DBConnectionString)
	if err != nil {
		return nil, fmt.Errorf("failed to create Postgres pool: %w", err)
	}
	return &Installation{Config: cfg, DB: stdlib.OpenDBFromPool(pool), Pool: pool}, nil
}

func (i *Installation) Close() {
	_ = i.DB.Close()
	if i.Pool != nil {
		i.Pool.Close()
	}
}

func (i *Installation) TableExists(ctx context.Context, table string) (exists bool, err error) {
	query := `SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = $1)`
	if i.Config.Postgres {
		query = `SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = current_schema() AND tablename = $1)`
	}
	err = i.DB.QueryRowContext(ctx, query, table).Scan(&exists)
	return exists, err
}

// OpenFileStorage opens the storage backend Pocket ID is configured with
func (i *Installation) OpenFileStorage(ctx context.Context) (storage.FileStorage, error) {
	cfg := i.Config
	switch cfg.FileBackend {
	case "s3":
		return storage.NewS3Storage(ctx, storage.S3Config{
			Bucket:                        cfg.S3Bucket,
			Region:                        cfg.S3Region,
			Endpoint:                      cfg.S3Endpoint,
			AccessKeyID:                   cfg.S3AccessKeyID,
			SecretAccessKey:               cfg.S3SecretAccessKey,
			ForcePathStyle:                cfg.S3ForcePathStyle,
			DisableDefaultIntegrityChecks: cfg.S3DisableDefaultIntegrityChecks,
			Root:                          cfg.UploadPath,
		})
	case "database":
		return storage.NewDatabaseStorage(i.DB, cfg.Postgres), nil
	default:
		return storage.NewFilesystemStorage(cfg.UploadPath)
	}
}

// addSqliteDatetimeParams adds the datetime parameters Pocket ID sets on its SQLite connection, leaving any the user set explicitly alone
func addSqliteDatetimeParams(connString string) string {
	path, rawQuery, _ := strings.Cut(connString, "?")
	if path == "" {
		// Return the connection string so the driver reports an error
		return connString
	}

	qs, err := url.ParseQuery(rawQuery)
	if err != nil {
		// Return the connection string so the driver reports an error
		return connString
	}

	for k, v := range map[string]string{
		"_texttotime":  "1",
		"_inttotime":   "1",
		"_time_format": "sqlite",
	} {
		if len(qs[k]) == 0 {
			qs.Set(k, v)
		}
	}

	return path + "?" + qs.Encode()
}
