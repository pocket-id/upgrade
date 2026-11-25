// Package tov3 brings a database of Pocket ID v1 or v2 to the form Pocket ID v3 starts from
package tov3

import (
	"context"
	"database/sql"
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
	err = warnAboutSharedEmails(ctx, db, postgres)
	if err != nil {
		return err
	}
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

	// Move the GeoLite database to the cache folder, which is not worth failing the upgrade over because Pocket ID runs without it
	err = moveGeoLiteDB(inst.Config)
	if err != nil {
		slog.Warn("Failed to move the GeoLite database to the cache folder, move it there by hand or let Pocket ID download it again", slog.String("from", legacyGeoLiteDBPath), slog.String("cacheFolder", inst.Config.CachePath), slog.Any("error", err))
	}

	// Move the actor data into its own SQLite file first, so the next step reads and writes it where v3 expects it
	err = moveActorData(ctx, inst)
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

// warnAboutSharedEmails names the accounts that hold one email address in different letter case, since v3 compares addresses without it and the migrations take the address from all but one of them
func warnAboutSharedEmails(ctx context.Context, db *sql.DB, postgres bool) error {
	// SQLite folds ASCII letters only, which Postgres does under the C collation
	folded := "lower(email)"
	if postgres {
		folded = `lower(email COLLATE "C")`
	}
	rows, err := db.QueryContext(ctx, `SELECT username FROM users WHERE `+folded+` IN (SELECT `+folded+` FROM users GROUP BY 1 HAVING count(*) > 1) ORDER BY username`)
	if err != nil {
		return fmt.Errorf("failed to look for accounts that share an email address: %w", err)
	}
	defer rows.Close()

	var usernames []string
	for rows.Next() {
		var username string
		err = rows.Scan(&username)
		if err != nil {
			return fmt.Errorf("failed to read an account that shares an email address: %w", err)
		}
		usernames = append(usernames, username)
	}
	err = rows.Err()
	if err != nil {
		return fmt.Errorf("failed to read the accounts that share an email address: %w", err)
	}

	if len(usernames) > 0 {
		slog.Warn("Some accounts share an email address apart from its letter case, which Pocket ID v3 no longer allows: the address stays with the verified account, or else with the oldest, and the others are left without an email", "usernames", usernames)
	}
	return nil
}
