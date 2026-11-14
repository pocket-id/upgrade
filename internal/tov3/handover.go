package tov3

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pressly/goose/v3/database"
)

// The version of the last migration in the migrations folder, which is also the version of v3's goose baseline
const baselineVersion = 20261114130000

// handOverToGoose replaces the version table of golang-migrate with the one of goose, marking v3's baseline as applied
// Both happen in one transaction, so a database either still needs this program or is ready for v3
func handOverToGoose(ctx context.Context, db *sql.DB, postgres bool) error {
	dialect := database.DialectSQLite3
	if postgres {
		dialect = database.DialectPostgres
	}
	store, err := database.NewStore(dialect, "goose_db_version")
	if err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Goose itself starts a new version table with version 0
	err = store.CreateVersionTable(ctx, tx)
	if err != nil {
		return fmt.Errorf("failed to create the goose version table: %w", err)
	}
	for _, version := range []int64{0, baselineVersion} {
		err = store.Insert(ctx, tx, database.InsertRequest{Version: version})
		if err != nil {
			return fmt.Errorf("failed to insert version %d: %w", version, err)
		}
	}

	_, err = tx.ExecContext(ctx, `DROP TABLE schema_migrations`)
	if err != nil {
		return fmt.Errorf("failed to drop the golang-migrate version table: %w", err)
	}
	return tx.Commit()
}
