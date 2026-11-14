package tov3

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	postgresMigrate "github.com/golang-migrate/migrate/v4/database/postgres"
	sqliteMigrate "github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"golang.org/x/text/unicode/norm"
	"modernc.org/sqlite"
)

// The migrations of every Pocket ID version before v3, as they were when v3 switched to goose
//
//go:embed migrations
var migrationsFS embed.FS

// The "normalize" migration uses `normalize(text, form)` for Unicode normalization, which SQLite only has once it is registered before the first connection
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("normalize", 2, func(ctx *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		arg0, ok := args[0].(string)
		if !ok {
			return nil, fmt.Errorf("first argument for normalize is not a string: %T", args[0])
		}

		arg1, ok := args[1].(string)
		if !ok {
			return nil, fmt.Errorf("second argument for normalize is not a string: %T", args[1])
		}

		var form norm.Form
		switch strings.ToLower(arg1) {
		case "nfc":
			form = norm.NFC
		case "nfd":
			form = norm.NFD
		case "nfkc":
			form = norm.NFKC
		case "nfkd":
			form = norm.NFKD
		default:
			return nil, fmt.Errorf("unsupported form: %s", arg1)
		}

		return form.String(arg0), nil
	})
}

// migrateLegacy applies the golang-migrate migrations the database doesn't have yet
func migrateLegacy(ctx context.Context, db *sql.DB, postgres bool) error {
	// Create the driver the way Pocket ID did, so the migrations run under the same conditions as in the versions that shipped them
	var (
		driver  database.Driver
		dialect string
		err     error
	)
	if postgres {
		dialect = "postgres"

		// The Postgres driver keeps the connection it is given, so it gets one of its own that returns to the pool afterwards
		var conn *sql.Conn
		conn, err = db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("failed to acquire migration connection: %w", err)
		}
		defer conn.Close()
		driver, err = postgresMigrate.WithConnection(ctx, conn, &postgresMigrate.Config{})
	} else {
		dialect = "sqlite"
		driver, err = sqliteMigrate.WithInstance(db, &sqliteMigrate.Config{NoTxWrap: true})
	}
	if err != nil {
		return fmt.Errorf("failed to create migration driver: %w", err)
	}

	source, err := iofs.New(migrationsFS, "migrations/"+dialect)
	if err != nil {
		return fmt.Errorf("failed to create migration source: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "pocket-id", driver)
	if err != nil {
		return fmt.Errorf("failed to create migration instance: %w", err)
	}

	err = m.Up()
	switch {
	case errors.Is(err, migrate.ErrNoChange):
		return nil
	case errors.As(err, &migrate.ErrDirty{}):
		return fmt.Errorf("a migration of an earlier Pocket ID version failed halfway, restore a backup taken before it and try again: %w", err)
	case err != nil:
		return fmt.Errorf("failed to apply migrations: %w", err)
	}
	return nil
}
