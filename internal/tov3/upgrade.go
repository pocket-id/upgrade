// Package tov3 brings a database of Pocket ID v1 or v2 to the form Pocket ID v3 starts from
package tov3

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pocket-id/upgrade/internal/install"
)

// Upgrade brings the installation to v3, and does nothing for one that is already there
// Each step leaves alone what is already done, so a run that was interrupted is simply repeated
func Upgrade(ctx context.Context, inst *install.Installation) error {
	db, postgres := inst.DB, inst.Config.Postgres

	// The last step drops the version table of golang-migrate, so without it there is nothing left to do
	legacy, err := inst.TableExists(ctx, "schema_migrations")
	if err != nil {
		return fmt.Errorf("failed to look for the golang-migrate version table: %w", err)
	}
	if !legacy {
		return nil
	}

	// The handover creates the goose table and drops the golang-migrate one together, so both only exist after an earlier Pocket ID version was started against an upgraded database
	// That version fails on its first migration and changes nothing else, but the table it leaves behind would send every start of v3 back here
	upgraded, err := inst.TableExists(ctx, "goose_db_version")
	if err != nil {
		return fmt.Errorf("failed to look for the goose version table: %w", err)
	}
	if upgraded {
		slog.Warn("An earlier Pocket ID version was started against this database after it was upgraded, removing the version table it left behind")
		_, err = db.ExecContext(ctx, `DROP TABLE schema_migrations`)
		if err != nil {
			return fmt.Errorf("failed to drop the golang-migrate version table: %w", err)
		}
		return nil
	}

	// Bring the schema to the last version before v3, which is what every later step and v3's first migration build on
	slog.Info("Upgrading to Pocket ID v3")
	err = migrateLegacy(ctx, db, postgres)
	if err != nil {
		return err
	}

	// Move the stored files, using the storage backend Pocket ID is configured with
	files, err := inst.OpenFileStorage(ctx)
	if err != nil {
		return fmt.Errorf("failed to open the file storage: %w", err)
	}
	err = upgradeStorage(ctx, db, files)
	if err != nil {
		return err
	}

	// Move the app config and the signup tokens into the actor state
	err = inst.WithStateStore(ctx, func(store install.StateStore) error {
		return upgradeActorState(ctx, db, store, inst.Config.EncryptionKey)
	})
	if err != nil {
		return err
	}

	// Hand the database over to goose last, since that is what marks the upgrade as done
	err = handOverToGoose(ctx, db, postgres)
	if err != nil {
		return fmt.Errorf("failed to hand the database over to goose: %w", err)
	}
	slog.Info("Upgraded to Pocket ID v3")
	return nil
}
