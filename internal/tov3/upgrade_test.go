package tov3

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/italypaleale/francis/actor"
	"github.com/italypaleale/francis/components"
	francispostgres "github.com/italypaleale/francis/components/postgres"
	francissqlite "github.com/italypaleale/francis/components/sqlite"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pocket-id/upgrade/internal/install"
)

const postgresURLEnv = "POCKET_ID_TEST_POSTGRES_URL"

func forEachDialect(t *testing.T, fn func(t *testing.T, postgres bool)) {
	t.Run("sqlite", func(t *testing.T) {
		fn(t, false)
	})
	t.Run("postgres", func(t *testing.T) {
		if os.Getenv(postgresURLEnv) == "" {
			t.Skip(postgresURLEnv + " is not set")
		}
		fn(t, true)
	})
}

// newTestDatabase creates an empty database with an empty filesystem storage, and connects to it
func newTestDatabase(t *testing.T, postgres bool) *install.Installation {
	t.Helper()

	cfg := &install.Config{
		EncryptionKey:      []byte("0123456789abcdef0123456789abcdef"),
		DBConnectionString: t.TempDir(),
		Postgres:           postgres,
		FileBackend:        "filesystem",
		UploadPath:         t.TempDir(),
	}
	if postgres {
		// Each test database gets a random name, so tests can run against one server at the same time
		admin, err := sql.Open("pgx", os.Getenv(postgresURLEnv))
		require.NoError(t, err)
		name := "v3_upgrade_" + strings.ToLower(rand.Text())
		_, err = admin.ExecContext(t.Context(), "CREATE DATABASE "+name)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = admin.ExecContext(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
			_ = admin.Close()
		})

		u, err := url.Parse(os.Getenv(postgresURLEnv))
		require.NoError(t, err)
		u.Path = "/" + name
		cfg.DBConnectionString = u.String()
	}

	inst, err := install.Open(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(inst.Close)
	return inst
}

// newLegacyDatabase creates a database in the state the last version before v3 leaves it in
func newLegacyDatabase(t *testing.T, postgres bool) *install.Installation {
	t.Helper()

	inst := newTestDatabase(t, postgres)
	require.NoError(t, migrateLegacy(t.Context(), inst.DB, postgres))
	return inst
}

// newDatabaseAt creates a database in the state the given migration of an earlier version left it in
func newDatabaseAt(t *testing.T, postgres bool, version uint) *install.Installation {
	t.Helper()

	inst := newTestDatabase(t, postgres)
	m, release, err := newLegacyMigrator(t.Context(), inst.DB, postgres)
	require.NoError(t, err)
	defer release()
	require.NoError(t, m.Migrate(version))
	return inst
}

func newBaselineProvider(t *testing.T, db *sql.DB, postgres bool) *goose.Provider {
	t.Helper()

	dialect, dir := goose.DialectSQLite3, "testdata/baseline/sqlite"
	if postgres {
		dialect, dir = goose.DialectPostgres, "testdata/baseline/postgres"
	}
	provider, err := goose.NewProvider(dialect, db, os.DirFS(dir))
	require.NoError(t, err)
	return provider
}

// readSchema describes every object of Pocket ID's schema, leaving out the tables of the migration tools and of Francis
func readSchema(t *testing.T, db *sql.DB, postgres bool) []string {
	t.Helper()

	// A dropped column leaves a gap in the column numbers of Postgres, so only the order of the columns is compared
	query := `
		SELECT type || ' ' || name || ' ' || COALESCE(sql, '') FROM sqlite_master
		WHERE tbl_name NOT LIKE 'goose_%' AND tbl_name NOT LIKE 'francis_%' AND tbl_name NOT LIKE 'sqlite_%'
		ORDER BY 1`
	if postgres {
		query = `
			SELECT 'column ' || table_name || '.' || column_name || ' ' || ROW_NUMBER() OVER (PARTITION BY table_name ORDER BY ordinal_position) || ' ' || udt_name || ' ' || COALESCE(character_maximum_length::text, '') || ' ' || is_nullable || ' ' || COALESCE(column_default, '')
			FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name NOT LIKE 'goose_%' AND table_name NOT LIKE 'francis_%'
			UNION ALL
			SELECT 'index ' || indexdef FROM pg_indexes
			WHERE schemaname = current_schema() AND tablename NOT LIKE 'goose_%' AND tablename NOT LIKE 'francis_%'
			UNION ALL
			SELECT 'constraint ' || conrelid::regclass || ' ' || conname || ' ' || pg_get_constraintdef(oid) FROM pg_constraint
			WHERE connamespace = current_schema()::regnamespace AND conrelid::regclass::text NOT LIKE 'goose_%' AND conrelid::regclass::text NOT LIKE 'francis_%'
			ORDER BY 1`
	}

	rows, err := db.QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer rows.Close()

	var schema []string
	for rows.Next() {
		var object string
		require.NoError(t, rows.Scan(&object))
		schema = append(schema, object)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, schema)
	return schema
}

// withLegacyStateStore calls fn with access to the actor state where v2 kept it, which on SQLite is the main database file
func withLegacyStateStore(t *testing.T, inst *install.Installation, fn func(store install.StateStore) error) error {
	t.Helper()

	legacy := *inst
	if !inst.Config.Postgres {
		legacy.ActorDB = inst.DB
	}
	return legacy.WithStateStore(t.Context(), fn)
}

func readState(t *testing.T, inst *install.Installation, actorType string, actorID string, dest any) error {
	t.Helper()

	var stateErr error
	err := inst.WithStateStore(t.Context(), func(store install.StateStore) error {
		stateErr = store.GetState(t.Context(), actorType, actorID, dest)
		return nil
	})
	require.NoError(t, err)
	return stateErr
}

// decryptSecret is the decryption of Pocket ID v3 for the secrets of the app config
func decryptSecret(t *testing.T, encryptionKey []byte, value string) string {
	t.Helper()

	encoded, ok := strings.CutPrefix(value, encryptedSecretPrefix)
	require.True(t, ok, "the secret is not encrypted")
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)

	key, err := hkdf.Key(sha256.New, encryptionKey, nil, "pocketid/encrypted_string", 32)
	require.NoError(t, err)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	aead, err := cipher.NewGCM(block)
	require.NoError(t, err)

	plaintext, err := aead.Open(nil, ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():], []byte("encrypted_string"))
	require.NoError(t, err)
	return string(plaintext)
}

func TestUpgradeHandsOverToGoose(t *testing.T) {
	forEachDialect(t, func(t *testing.T, postgres bool) {
		inst := newLegacyDatabase(t, postgres)
		db := inst.DB
		require.NoError(t, Upgrade(t.Context(), inst))

		// Goose must consider the baseline applied, or v3 would run it on top of the existing tables
		pending, err := newBaselineProvider(t, db, postgres).HasPending(t.Context())
		require.NoError(t, err)
		assert.False(t, pending)

		// A new v3 install must end up with the schema an upgraded one has
		fresh := newTestDatabase(t, postgres).DB
		_, err = newBaselineProvider(t, fresh, postgres).Up(t.Context())
		require.NoError(t, err)
		assert.Equal(t, readSchema(t, db, postgres), readSchema(t, fresh, postgres))

		// A database that was handed over has nothing left to upgrade
		require.NoError(t, Upgrade(t.Context(), inst))

		// An earlier Pocket ID version started against it fails and leaves its version table behind, which must not keep v3 from starting
		require.Error(t, migrateLegacy(t.Context(), db, postgres))
		require.NoError(t, Upgrade(t.Context(), inst))
		legacy, err := inst.TableExists(t.Context(), "schema_migrations")
		require.NoError(t, err)
		assert.False(t, legacy)
		assert.Equal(t, readSchema(t, db, postgres), readSchema(t, fresh, postgres))
	})
}

func TestMigrationKeepsAdminAsActorOfPasskeyRemoval(t *testing.T) {
	forEachDialect(t, func(t *testing.T, postgres bool) {
		// Start from the last schema without the actor columns
		db := newDatabaseAt(t, postgres, 20261108120000).DB
		const (
			ownerID = "9a1c1e0e-0b7c-4b7e-9d0f-2f1c8f0a1b01"
			adminID = "9a1c1e0e-0b7c-4b7e-9d0f-2f1c8f0a1b02"
		)
		for id, username := range map[string]string{ownerID: "owner", adminID: "admin"} {
			_, err := db.ExecContext(t.Context(), `INSERT INTO users (id, username, first_name, last_name, display_name) VALUES ($1, $2, '', '', '')`, id, username)
			require.NoError(t, err)
		}
		for id, data := range map[string]string{
			"9a1c1e0e-0b7c-4b7e-9d0f-2f1c8f0a1b03": `{"actorUserID":"` + adminID + `","actorUsername":"admin"}`,
			"9a1c1e0e-0b7c-4b7e-9d0f-2f1c8f0a1b04": `{}`,
		} {
			_, err := db.ExecContext(t.Context(), `INSERT INTO audit_logs (id, created_at, event, user_agent, data, user_id) VALUES ($1, CURRENT_TIMESTAMP, 'PASSKEY_REMOVED', '', $2, $3)`, id, []byte(data), ownerID)
			require.NoError(t, err)
		}

		require.NoError(t, migrateLegacy(t.Context(), db, postgres))

		// The admin stays the actor of the removal on the owner's account, and the owner the actor of their own
		rows, err := db.QueryContext(t.Context(), `SELECT actor_name FROM audit_logs ORDER BY id`)
		require.NoError(t, err)
		defer rows.Close()
		var actors []string
		for rows.Next() {
			var actor string
			require.NoError(t, rows.Scan(&actor))
			actors = append(actors, actor)
		}
		require.NoError(t, rows.Err())
		assert.Equal(t, []string{"admin", "owner"}, actors)
	})
}

func TestUpgradeLeavesSharedEmailWithOneAccount(t *testing.T) {
	forEachDialect(t, func(t *testing.T, postgres bool) {
		// Start from the last schema that compares the letter case of an email
		inst := newDatabaseAt(t, postgres, 20261114125700)
		db := inst.DB
		for _, user := range []struct {
			id, username, email string
			verified            bool
		}{
			{"9a1c1e0e-0b7c-4b7e-9d0f-2f1c8f0a1b01", "unverified", "Kim@example.com", false},
			{"9a1c1e0e-0b7c-4b7e-9d0f-2f1c8f0a1b02", "verified", "kim@EXAMPLE.com", true},
			{"9a1c1e0e-0b7c-4b7e-9d0f-2f1c8f0a1b03", "older", "Lee@example.com", false},
			{"9a1c1e0e-0b7c-4b7e-9d0f-2f1c8f0a1b04", "newer", "lee@example.com", false},
		} {
			_, err := db.ExecContext(t.Context(), `INSERT INTO users (id, username, email, email_verified, first_name, last_name, display_name) VALUES ($1, $2, $3, $4, '', '', '')`, user.id, user.username, user.email, user.verified)
			require.NoError(t, err)
		}

		require.NoError(t, Upgrade(t.Context(), inst))

		// The verified account keeps the address, and without one the first account does
		rows, err := db.QueryContext(t.Context(), `SELECT username FROM users WHERE email IS NOT NULL ORDER BY username`)
		require.NoError(t, err)
		defer rows.Close()
		var holders []string
		for rows.Next() {
			var holder string
			require.NoError(t, rows.Scan(&holder))
			holders = append(holders, holder)
		}
		require.NoError(t, rows.Err())
		assert.Equal(t, []string{"older", "verified"}, holders)

		// Another spelling of a held address is refused from now on
		_, err = db.ExecContext(t.Context(), `UPDATE users SET email = 'LEE@example.com' WHERE username = 'newer'`)
		require.Error(t, err)
	})
}

func TestUpgradeStorage(t *testing.T) {
	legacyLogo, err := os.ReadFile("testdata/logoLight.svg")
	require.NoError(t, err)

	forEachDialect(t, func(t *testing.T, postgres bool) {
		for _, backend := range []string{"filesystem", "database"} {
			t.Run(backend, func(t *testing.T) {
				inst := newLegacyDatabase(t, postgres)
				db := inst.DB
				inst.Config.FileBackend = backend
				files, err := inst.OpenFileStorage(t.Context())
				require.NoError(t, err)

				// Store the files the way a version before the folder was renamed left them, with a deleted background
				const cimdClientID = "https://client.example.com/metadata.json"
				for path, content := range map[string]string{
					"application-images/.inited":                                   "",
					"application-images/logoLight.svg":                             string(legacyLogo),
					"application-images/logoDark.svg":                              "custom logo",
					"oidc-client-images/client.png":                                "light",
					"oidc-client-images/client-dark.svg":                           "dark",
					"oidc-client-images/cimd-" + sha256Hex(cimdClientID) + ".webp": "cimd",
					"profile-pictures/defaults/AL.png":                             "default picture",
					"profile-pictures/user.png":                                    "uploaded picture",
				} {
					require.NoError(t, files.Save(t.Context(), path, strings.NewReader(content)))
				}
				for id, imageTypes := range map[string][2]any{
					"client":      {"png", "svg"},
					cimdClientID:  {"webp", nil},
					"no-logo":     {nil, nil},
					"lost-upload": {"png", nil},
				} {
					_, err = db.ExecContext(t.Context(), `INSERT INTO oidc_clients (id, created_at, image_type, dark_image_type) VALUES ($1, CURRENT_TIMESTAMP, $2, $3)`, id, imageTypes[0], imageTypes[1])
					require.NoError(t, err)
				}

				listFiles := func() []string {
					var all []string
					for _, root := range []string{legacyApplicationAssetsPath, applicationAssetsPath, oidcClientImagesPath, "profile-pictures"} {
						paths, err := files.List(t.Context(), root)
						require.NoError(t, err)
						all = append(all, paths...)
					}
					slices.Sort(all)
					return all
				}
				expected := []string{
					"application-assets/logoDark.svg",
					"oidc-client-images/" + sha256Hex(cimdClientID) + ".webp",
					"oidc-client-images/" + sha256Hex("client") + "-dark.svg",
					"oidc-client-images/" + sha256Hex("client") + ".png",
					"profile-pictures/user.png",
				}
				slices.Sort(expected)

				require.NoError(t, Upgrade(t.Context(), inst))
				assert.Equal(t, expected, listFiles())

				reader, err := files.Open(t.Context(), "application-assets/logoDark.svg")
				require.NoError(t, err)
				content, err := io.ReadAll(reader)
				require.NoError(t, err)
				require.NoError(t, reader.Close())
				assert.Equal(t, "custom logo", string(content))

				// The custom logo is recorded, the deleted background is recorded as empty, and the default logo of the earlier version is left for v3 to restore
				listAssetRows := func() map[string]string {
					rows, err := db.QueryContext(t.Context(), `SELECT "key", value FROM kv WHERE "key" LIKE 'application_asset_%'`)
					require.NoError(t, err)
					defer rows.Close()
					assets := map[string]string{}
					for rows.Next() {
						var key, value string
						require.NoError(t, rows.Scan(&key, &value))
						assets[key] = value
					}
					require.NoError(t, rows.Err())
					return assets
				}
				expectedRows := map[string]string{
					"application_asset_background": "",
					"application_asset_logoDark":   "logoDark.svg",
				}
				assert.Equal(t, expectedRows, listAssetRows())

				// A run that repeats the step after an interruption must leave the result alone
				require.NoError(t, upgradeStorage(t.Context(), db, files))
				assert.Equal(t, expected, listFiles())
				assert.Equal(t, expectedRows, listAssetRows())
			})
		}
	})
}

func TestUpgradeActorStateFromKV(t *testing.T) {
	forEachDialect(t, func(t *testing.T, postgres bool) {
		inst := newLegacyDatabase(t, postgres)
		db := inst.DB

		// Store the app config and the signup tokens the way the versions without actors left them in the kv table
		expiresAt := time.Now().Add(time.Hour).Truncate(time.Second)
		createdAt := time.Now().Add(-time.Hour).Truncate(time.Second)
		signupTokens, err := json.Marshal([]map[string]any{
			{"id": "valid-id", "token": "valid-token", "expiresAt": expiresAt.Unix(), "usageLimit": 5, "usageCount": 2, "userGroupIds": []string{"group"}, "createdAt": createdAt.Unix()},
			{"id": "expired-id", "token": "expired-token", "expiresAt": createdAt.Unix(), "usageLimit": 1, "usageCount": 0, "createdAt": createdAt.Unix()},
		})
		require.NoError(t, err)
		for key, value := range map[string]string{
			legacyConfigKey:       `{"appName":"Legacy ID","smtpPassword":"smtp-secret","ldapBindPassword":"","emailVerificationEnabled":"true","emailLoginNotificationEnabled":"false","removedSetting":"value"}`,
			legacySignupTokensKey: string(signupTokens),
		} {
			_, err = db.ExecContext(t.Context(), `INSERT INTO kv ("key", value) VALUES ($1, $2)`, key, value)
			require.NoError(t, err)
		}

		require.NoError(t, Upgrade(t.Context(), inst))

		// The switches became modes and the secret is encrypted, while empty and unknown settings are left to v3's defaults
		var appConfig map[string]string
		require.NoError(t, readState(t, inst, appConfigActorType, actor.SingletonActorID, &appConfig))
		assert.Equal(t, "smtp-secret", decryptSecret(t, inst.Config.EncryptionKey, appConfig["SmtpPassword"]))
		delete(appConfig, "SmtpPassword")
		assert.Equal(t, map[string]string{
			"AppName":                    "Legacy ID",
			"EmailVerification":          "optional",
			"EmailLoginNotificationMode": "disabled",
		}, appConfig)

		var token signupTokenState
		require.NoError(t, readState(t, inst, signupTokenActorType, hashToken("valid-token"), &token))
		assert.Equal(t, "valid-id", token.ID)
		assert.Equal(t, "vali", token.Prefix)
		assert.Equal(t, 5, token.UsageLimit)
		assert.Equal(t, 2, token.UsageCount)
		assert.Equal(t, []string{"group"}, token.UserGroupIDs)
		assert.True(t, createdAt.Equal(token.CreatedAt))
		assert.True(t, expiresAt.Equal(token.ExpiresAt))

		err = readState(t, inst, signupTokenActorType, hashToken("expired-token"), &signupTokenState{})
		require.ErrorIs(t, err, actor.ErrStateNotFound)

		var legacyRows int
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM kv WHERE "key" IN ($1, $2)`, legacyConfigKey, legacySignupTokensKey).Scan(&legacyRows))
		assert.Zero(t, legacyRows)
	})
}

func TestUpgradeActorStateFromState(t *testing.T) {
	forEachDialect(t, func(t *testing.T, postgres bool) {
		inst := newLegacyDatabase(t, postgres)
		db := inst.DB

		// Restore the actor state of a v2 instance, whose app config has the two switches and a plaintext SMTP password
		var provider components.ActorProvider
		var err error
		if postgres {
			provider, err = francispostgres.NewPostgresProvider(slog.Default(), francispostgres.PostgresProviderOptions{DB: inst.Pool}, components.NewProviderConfig())
		} else {
			provider, err = francissqlite.NewSQLiteProvider(slog.Default(), francissqlite.SQLiteProviderOptions{DB: db}, components.NewProviderConfig())
		}
		require.NoError(t, err)
		require.NoError(t, provider.Init(t.Context()))
		snapshot, err := os.Open("testdata/francis.bin")
		require.NoError(t, err)
		defer snapshot.Close()
		require.NoError(t, provider.Restore(t.Context(), snapshot))
		require.NoError(t, provider.Close())

		var before map[string]string
		err = withLegacyStateStore(t, inst, func(store install.StateStore) error {
			return store.GetState(t.Context(), appConfigActorType, actor.SingletonActorID, &before)
		})
		require.NoError(t, err)
		require.Equal(t, "true", before["EmailVerificationEnabled"])
		require.Equal(t, "false", before["EmailLoginNotificationEnabled"])
		require.NotEmpty(t, before["SmtpPassword"])

		// The kv row that every v2 version kept must lose against the state
		_, err = db.ExecContext(t.Context(), `INSERT INTO kv ("key", value) VALUES ($1, $2)`, legacyConfigKey, `{"appName":"Outdated"}`)
		require.NoError(t, err)

		require.NoError(t, Upgrade(t.Context(), inst))

		var after map[string]string
		require.NoError(t, readState(t, inst, appConfigActorType, actor.SingletonActorID, &after))
		assert.Equal(t, before["SmtpPassword"], decryptSecret(t, inst.Config.EncryptionKey, after["SmtpPassword"]))

		// On SQLite the actor data moved into its own file, leaving nothing of Francis in the main one
		if !postgres {
			var francisObjects int
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'francis\_%' ESCAPE '\'`).Scan(&francisObjects))
			assert.Zero(t, francisObjects)
			require.NoError(t, moveActorData(t.Context(), inst))
		}

		// Everything but the switches and the secret must be carried over unchanged
		expected := before
		delete(expected, "EmailVerificationEnabled")
		delete(expected, "EmailLoginNotificationEnabled")
		expected["EmailVerification"] = "optional"
		expected["EmailLoginNotificationMode"] = "disabled"
		expected["SmtpPassword"] = after["SmtpPassword"]
		assert.Equal(t, expected, after)

		// A run that repeats the step after an interruption must not encrypt the secret a second time
		err = inst.WithStateStore(t.Context(), func(store install.StateStore) error {
			return upgradeActorState(t.Context(), db, store, inst.Config.EncryptionKey)
		})
		require.NoError(t, err)
		var rerun map[string]string
		require.NoError(t, readState(t, inst, appConfigActorType, actor.SingletonActorID, &rerun))
		assert.Equal(t, after, rerun)
	})
}

func TestUpgradeActorStateHashesTokens(t *testing.T) {
	forEachDialect(t, func(t *testing.T, postgres bool) {
		inst := newLegacyDatabase(t, postgres)

		// Store the tokens the way v2 did, keyed by the token itself and with the device token in clear text
		expiresAt := time.Now().Add(time.Hour).Truncate(time.Second)
		err := withLegacyStateStore(t, inst, func(store install.StateStore) error {
			err := store.SetState(t.Context(), signupTokenActorType, "SIGNUPTOKEN12345", signupTokenState{ID: "signup-id", UsageLimit: 3, UsageCount: 1, ExpiresAt: expiresAt}, &actor.SetStateOpts{TTL: time.Hour})
			if err != nil {
				return err
			}
			return store.SetState(t.Context(), oneTimeAccessTokenActorType, "abc123", oneTimeAccessTokenState{UserID: "user-id", DeviceToken: new("device-token"), ExpiresAt: expiresAt}, &actor.SetStateOpts{TTL: time.Hour})
		})
		require.NoError(t, err)

		require.NoError(t, Upgrade(t.Context(), inst))

		// Only the hashes are left, and the states carry over with the prefix and the hashed device token
		var signupToken signupTokenState
		require.NoError(t, readState(t, inst, signupTokenActorType, hashToken("SIGNUPTOKEN12345"), &signupToken))
		assert.Equal(t, "signup-id", signupToken.ID)
		assert.Equal(t, "SIGN", signupToken.Prefix)
		assert.Equal(t, 1, signupToken.UsageCount)
		assert.True(t, expiresAt.Equal(signupToken.ExpiresAt))
		require.ErrorIs(t, readState(t, inst, signupTokenActorType, "SIGNUPTOKEN12345", &signupTokenState{}), actor.ErrStateNotFound)

		var loginCode oneTimeAccessTokenState
		require.NoError(t, readState(t, inst, oneTimeAccessTokenActorType, hashToken("abc123"), &loginCode))
		assert.Equal(t, "user-id", loginCode.UserID)
		assert.Equal(t, hashToken("device-token"), *loginCode.DeviceToken)
		require.ErrorIs(t, readState(t, inst, oneTimeAccessTokenActorType, "abc123", &oneTimeAccessTokenState{}), actor.ErrStateNotFound)

		// A repeated run must not hash the hashes again
		err = inst.WithStateStore(t.Context(), func(store install.StateStore) error {
			return upgradeActorState(t.Context(), inst.DB, store, inst.Config.EncryptionKey)
		})
		require.NoError(t, err)
		var rerun oneTimeAccessTokenState
		require.NoError(t, readState(t, inst, oneTimeAccessTokenActorType, hashToken("abc123"), &rerun))
		assert.Equal(t, loginCode, rerun)
	})
}

func TestMoveGeoLiteDB(t *testing.T) {
	t.Chdir(t.TempDir())
	require.NoError(t, os.Mkdir("data", 0700))
	require.NoError(t, os.WriteFile(legacyGeoLiteDBPath, []byte("geolite"), 0600))

	// A path that was set explicitly keeps the database where it is
	require.NoError(t, moveGeoLiteDB(&install.Config{CachePath: "cache", GeoLiteDBPath: legacyGeoLiteDBPath}))
	require.FileExists(t, legacyGeoLiteDBPath)

	require.NoError(t, moveGeoLiteDB(&install.Config{CachePath: "cache"}))
	content, err := os.ReadFile("cache/GeoLite2-City.mmdb")
	require.NoError(t, err)
	assert.Equal(t, "geolite", string(content))
	require.NoFileExists(t, legacyGeoLiteDBPath)

	// Only the database is left in the cache folder, and a repeated run finds nothing to move
	entries, err := os.ReadDir("cache")
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	require.NoError(t, moveGeoLiteDB(&install.Config{CachePath: "cache"}))
}
