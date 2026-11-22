package tov3

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/italypaleale/francis/components"
	"github.com/italypaleale/francis/components/sqlite"

	"github.com/pocket-id/upgrade/internal/install"
)

// moveActorData moves the tables of the embedded Francis runtime out of the main SQLite file into the file of their own that v3 keeps them in, so the two never block each other's writes
// Postgres keeps them in Pocket ID's database, and a standalone runtime stores them itself
func moveActorData(ctx context.Context, inst *install.Installation) error {
	if inst.Config.Postgres || len(inst.Config.FrancisAddresses) > 0 {
		return nil
	}

	// Dropping the tables from the main file is the last step, so without them there is nothing left to move, and a database from before v2.10 never had them
	exists, err := inst.TableExists(ctx, "francis_metadata")
	if err != nil {
		return fmt.Errorf("failed to look for the Francis tables: %w", err)
	}
	if !exists {
		return nil
	}

	// Take a snapshot of the actor data, after Init brought the old tables to the schema this Francis version reads
	slog.Info("Moving the actor data into its own database file")
	var snapshot bytes.Buffer
	err = withSqliteActorProvider(ctx, inst.DB, func(provider components.ActorProvider) error {
		return provider.Backup(ctx, &snapshot)
	})
	if err != nil {
		return fmt.Errorf("failed to back up the actor data: %w", err)
	}

	// Restore wipes the new file first, so a run that was interrupted before the drop below simply restores again
	err = withSqliteActorProvider(ctx, inst.ActorDB, func(provider components.ActorProvider) error {
		return provider.Restore(ctx, &snapshot)
	})
	if err != nil {
		return fmt.Errorf("failed to restore the actor data into its own file: %w", err)
	}

	err = dropActorTables(ctx, inst.DB)
	if err != nil {
		return fmt.Errorf("failed to drop the actor tables from the main database file: %w", err)
	}
	return nil
}

// withSqliteActorProvider calls fn with a Francis provider on db, whose Init creates or upgrades the Francis tables
func withSqliteActorProvider(ctx context.Context, db *sql.DB, fn func(provider components.ActorProvider) error) error {
	provider, err := sqlite.NewSQLiteProvider(slog.Default().With("scope", "actor-data"), sqlite.SQLiteProviderOptions{DB: db}, components.NewProviderConfig())
	if err != nil {
		return fmt.Errorf("failed to create actor provider: %w", err)
	}
	defer provider.Close()

	err = provider.Init(ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize actor provider: %w", err)
	}
	return fn(provider)
}

// dropActorTables drops every Francis object from the main file in one transaction
func dropActorTables(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	// Views go before the tables they read, and the hosts table goes last, since dropping it first would delete the rows that reference it one by one
	// Indexes and triggers go with their tables
	rows, err := tx.QueryContext(ctx, `
		SELECT type, name FROM sqlite_master
		WHERE type IN ('view', 'table') AND name LIKE 'francis\_%' ESCAPE '\'
		ORDER BY type = 'table', name = 'francis_hosts'`)
	if err != nil {
		return err
	}
	var objects [][2]string
	for rows.Next() {
		var object [2]string
		err = rows.Scan(&object[0], &object[1])
		if err != nil {
			_ = rows.Close()
			return err
		}
		objects = append(objects, object)
	}
	err = rows.Close()
	if err != nil {
		return err
	}

	for _, object := range objects {
		// #nosec G202 -- the type is one of two keywords and the name comes from the schema
		_, err = tx.ExecContext(ctx, `DROP `+object[0]+` "`+object[1]+`"`)
		if err != nil {
			return fmt.Errorf("failed to drop %s %s: %w", object[0], object[1], err)
		}
	}
	return tx.Commit()
}
