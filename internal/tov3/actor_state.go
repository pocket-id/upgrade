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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/italypaleale/francis/actor"
	"github.com/italypaleale/go-kit/utils"

	"github.com/pocket-id/upgrade/internal/install"
)

const (
	appConfigActorType          = "AppConfig"
	signupTokenActorType        = "SignupToken"
	oneTimeAccessTokenActorType = "OneTimeAccessToken"

	// The kv rows that hold the app config and the signup tokens of the versions that kept them in tables
	legacyConfigKey       = "config_migrated"
	legacySignupTokensKey = "signup_tokens_migrated" // #nosec G101 -- database key name, not a credential

	encryptedSecretPrefix = "encrypted:"

	// signupTokenPrefixLength is how many leading characters of a signup token v3 keeps in clear text so admins can tell tokens apart
	signupTokenPrefixLength = 4
)

// The fields of v3's app config by the key earlier versions stored them under
// The last two are the switches that became modes, which only serve to derive the mode
var appConfigFields = map[string]string{
	"appName":                    "AppName",
	"sessionDuration":            "SessionDuration",
	"homePageUrl":                "HomePageURL",
	"emailsVerified":             "EmailsVerified",
	"appearance":                 "Appearance",
	"allowOwnAccountEdit":        "AllowOwnAccountEdit",
	"allowUserSignups":           "AllowUserSignups",
	"signupDefaultUserGroupIDs":  "SignupDefaultUserGroupIDs",
	"signupDefaultCustomClaims":  "SignupDefaultCustomClaims",
	"requireUserEmail":           "RequireUserEmail",
	"smtpHost":                   "SmtpHost",
	"smtpPort":                   "SmtpPort",
	"smtpFrom":                   "SmtpFrom",
	"smtpUser":                   "SmtpUser",
	"smtpPassword":               "SmtpPassword",
	"smtpTls":                    "SmtpTls",
	"smtpSkipCertVerify":         "SmtpSkipCertVerify",
	"emailLoginNotificationMode": "EmailLoginNotificationMode",
	"emailOneTimeAccessAsUnauthenticatedEnabled": "EmailOneTimeAccessAsUnauthenticatedEnabled",
	"emailOneTimeAccessAsAdminEnabled":           "EmailOneTimeAccessAsAdminEnabled",
	"emailApiKeyExpirationEnabled":               "EmailApiKeyExpirationEnabled",
	"emailVerification":                          "EmailVerification",
	"ldapEnabled":                                "LdapEnabled",
	"ldapUrl":                                    "LdapUrl",
	"ldapBindDn":                                 "LdapBindDn",
	"ldapBindPassword":                           "LdapBindPassword",
	"ldapBase":                                   "LdapBase",
	"ldapUserSearchFilter":                       "LdapUserSearchFilter",
	"ldapUserGroupSearchFilter":                  "LdapUserGroupSearchFilter",
	"ldapSkipCertVerify":                         "LdapSkipCertVerify",
	"ldapAttributeUserUniqueIdentifier":          "LdapAttributeUserUniqueIdentifier",
	"ldapAttributeUserUsername":                  "LdapAttributeUserUsername",
	"ldapAttributeUserEmail":                     "LdapAttributeUserEmail",
	"ldapAttributeUserFirstName":                 "LdapAttributeUserFirstName",
	"ldapAttributeUserLastName":                  "LdapAttributeUserLastName",
	"ldapAttributeUserDisplayName":               "LdapAttributeUserDisplayName",
	"ldapAttributeUserProfilePicture":            "LdapAttributeUserProfilePicture",
	"ldapAttributeGroupMember":                   "LdapAttributeGroupMember",
	"ldapAttributeGroupUniqueIdentifier":         "LdapAttributeGroupUniqueIdentifier",
	"ldapAttributeGroupName":                     "LdapAttributeGroupName",
	"ldapAdminGroupName":                         "LdapAdminGroupName",
	"ldapSoftDeleteUsers":                        "LdapSoftDeleteUsers",
	"webauthnUserVerification":                   "WebauthnUserVerification",
	"webauthnAllowSyncedPasskeys":                "WebauthnAllowSyncedPasskeys",
	"webauthnAuthenticatorAttachment":            "WebauthnAuthenticatorAttachment",
	"webauthnAttestation":                        "WebauthnAttestation",
	"cimdUrlAllowlist":                           "CIMDURLAllowlist",
	"autoCreateOidcClientSecret":                 "AutoCreateOIDCClientSecret",
	"enabledFeatures":                            "EnabledFeatures",
	"emailLoginNotificationEnabled":              "EmailLoginNotificationEnabled",
	"emailVerificationEnabled":                   "EmailVerificationEnabled",
}

// signupTokenState is the state of v3's SignupToken actor, whose ID is the SHA-256 hash of the token
type signupTokenState struct {
	ID           string
	Prefix       string
	UsageLimit   int
	UsageCount   int
	UserGroupIDs []string
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// oneTimeAccessTokenState is the state of v3's OneTimeAccessToken actor, whose ID is the SHA-256 hash of the token
type oneTimeAccessTokenState struct {
	UserID string
	// DeviceToken is the SHA-256 hash of the device token the token is bound to
	DeviceToken *string
	ExpiresAt   time.Time
}

// upgradeActorState brings the app config and the tokens into the actor state v3 reads
func upgradeActorState(ctx context.Context, db *sql.DB, store install.StateStore, encryptionKey []byte) error {
	err := upgradeAppConfig(ctx, db, store, encryptionKey)
	if err != nil {
		return fmt.Errorf("failed to upgrade the app config: %w", err)
	}

	// v2 keyed the token actors by the token itself, while v3 only stores hashes so the state store holds no usable token
	err = rekeyTokens(ctx, store, signupTokenActorType, func(token string, state *signupTokenState) time.Time {
		state.Prefix = token[:min(signupTokenPrefixLength, len(token))]
		return state.ExpiresAt
	})
	if err != nil {
		return fmt.Errorf("failed to hash the signup tokens: %w", err)
	}
	err = rekeyTokens(ctx, store, oneTimeAccessTokenActorType, func(_ string, state *oneTimeAccessTokenState) time.Time {
		if state.DeviceToken != nil {
			state.DeviceToken = new(hashToken(*state.DeviceToken))
		}
		return state.ExpiresAt
	})
	if err != nil {
		return fmt.Errorf("failed to hash the login codes: %w", err)
	}

	// The tokens in the kv table come last, so the check for a token that has state only needs to look under its hash
	err = upgradeSignupTokens(ctx, db, store)
	if err != nil {
		return fmt.Errorf("failed to upgrade the signup tokens: %w", err)
	}

	// The frozen signup tokens must go, since they would revive a token that is deleted later
	_, err = db.ExecContext(ctx, `DELETE FROM kv WHERE "key" IN ($1, $2)`, legacyConfigKey, legacySignupTokensKey)
	if err != nil {
		return fmt.Errorf("failed to delete the legacy kv rows: %w", err)
	}
	return nil
}

func upgradeAppConfig(ctx context.Context, db *sql.DB, store install.StateStore, encryptionKey []byte) error {
	// The actor state wins over the kv row, which no version deleted once the config had moved into the state
	// Every field of the config is stored as a string, so the state is handled as a map to keep the fields this program doesn't know
	state := map[string]string{}
	err := store.GetState(ctx, appConfigActorType, actor.SingletonActorID, &state)
	if errors.Is(err, actor.ErrStateNotFound) {
		var legacy map[string]string
		legacy, err = loadLegacyKV[map[string]string](ctx, db, legacyConfigKey)
		for key, value := range legacy {
			field, ok := appConfigFields[key]
			if ok && value != "" {
				state[field] = value
			}
		}
	}
	if err != nil {
		return err
	}

	// v3 creates a default config by itself when there is none
	if len(state) == 0 {
		return nil
	}

	// Two switches became settings with more than two modes, where the mode wins if a version already saved one
	for legacyField, mode := range map[string][2]string{
		"EmailLoginNotificationEnabled": {"EmailLoginNotificationMode", "browserRecognition"},
		"EmailVerificationEnabled":      {"EmailVerification", "optional"},
	} {
		if state[mode[0]] == "" && state[legacyField] != "" {
			state[mode[0]] = "disabled"
			if utils.IsTruthy(state[legacyField]) {
				state[mode[0]] = mode[1]
			}
		}
		delete(state, legacyField)
	}

	// v3 only reads secrets that are encrypted, which no earlier version did
	for _, field := range []string{"SmtpPassword", "LdapBindPassword"} {
		value := state[field]
		if value == "" || strings.HasPrefix(value, encryptedSecretPrefix) {
			continue
		}
		state[field], err = encryptSecret(encryptionKey, value)
		if err != nil {
			return fmt.Errorf("failed to encrypt %s: %w", field, err)
		}
	}

	return store.SetState(ctx, appConfigActorType, actor.SingletonActorID, state, nil)
}

func upgradeSignupTokens(ctx context.Context, db *sql.DB, store install.StateStore) error {
	// All timestamps are Unix seconds
	tokens, err := loadLegacyKV[[]struct {
		ID           string   `json:"id"`
		Token        string   `json:"token"`
		ExpiresAt    int64    `json:"expiresAt"`
		UsageLimit   int      `json:"usageLimit"`
		UsageCount   int      `json:"usageCount"`
		UserGroupIDs []string `json:"userGroupIds"`
		CreatedAt    int64    `json:"createdAt"`
	}](ctx, db, legacySignupTokensKey)
	if err != nil {
		return err
	}

	moved := 0
	for _, token := range tokens {
		// Skip tokens that have already expired, since there would be nothing left to store
		expiresAt := time.Unix(token.ExpiresAt, 0)
		if !expiresAt.After(time.Now()) {
			continue
		}

		// A token that already has state was moved by an earlier version or an interrupted run, and may have been used since
		err = store.GetState(ctx, signupTokenActorType, hashToken(token.Token), &signupTokenState{})
		if err == nil {
			continue
		} else if !errors.Is(err, actor.ErrStateNotFound) {
			return err
		}

		err = store.SetState(ctx, signupTokenActorType, hashToken(token.Token), signupTokenState{
			ID:           token.ID,
			Prefix:       token.Token[:min(signupTokenPrefixLength, len(token.Token))],
			UsageLimit:   token.UsageLimit,
			UsageCount:   token.UsageCount,
			UserGroupIDs: token.UserGroupIDs,
			CreatedAt:    time.Unix(token.CreatedAt, 0),
			ExpiresAt:    expiresAt,
		}, &actor.SetStateOpts{TTL: time.Until(expiresAt)})
		if err != nil {
			return fmt.Errorf("failed to store signup token '%s': %w", token.ID, err)
		}
		moved++
	}

	if moved > 0 {
		slog.Info("Moved signup tokens to the actor state", slog.Int("count", moved))
	}
	return nil
}

// rekeyTokens moves every state of the actor type that is keyed by a token to the token's hash, which is what v3 looks it up by
// fn turns the token and its state into the state v3 reads, and returns when that state expires
func rekeyTokens[T any](ctx context.Context, store install.StateStore, actorType string, fn func(token string, state *T) time.Time) error {
	// Collect every state before changing any, since a hashed ID could otherwise show up again further down the listing
	var states []actor.StateInfo
	var after string
	for {
		res, err := store.ListStates(ctx, actorType, &actor.ListStatesOpts{IncludeData: true, After: after})
		if err != nil {
			return fmt.Errorf("failed to list the states: %w", err)
		}
		states = append(states, res.States...)
		after = res.AfterID()
		if after == "" {
			break
		}
	}

	moved := 0
	for _, st := range states {
		// A hashed ID comes from an interrupted run, since no token is 64 hex characters long
		if st.Data == nil || isTokenHash(st.ActorID) {
			continue
		}

		var state T
		err := st.Data.Decode(&state)
		if err != nil {
			return fmt.Errorf("failed to decode the state of a token: %w", err)
		}

		// Store the hashed state before deleting the old one, so an interrupted run loses no token
		expiresAt := fn(st.ActorID, &state)
		ttl := time.Until(expiresAt)
		if ttl > 0 {
			err = store.SetState(ctx, actorType, hashToken(st.ActorID), state, &actor.SetStateOpts{TTL: ttl})
			if err != nil {
				return fmt.Errorf("failed to store the hashed state of a token: %w", err)
			}
		}
		err = store.DeleteState(ctx, actorType, st.ActorID)
		if err != nil && !errors.Is(err, actor.ErrStateNotFound) {
			return fmt.Errorf("failed to delete the state of a token: %w", err)
		}
		moved++
	}

	if moved > 0 {
		slog.Info("Hashed tokens in the actor state", slog.String("actorType", actorType), slog.Int("count", moved))
	}
	return nil
}

// hashToken returns the hex-encoded SHA-256 hash v3 uses as the actor ID of a token
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func isTokenHash(actorID string) bool {
	_, err := hex.DecodeString(actorID)
	return err == nil && len(actorID) == 2*sha256.Size
}

// loadLegacyKV returns the JSON value of a kv row, or the zero value if the row is missing or empty
func loadLegacyKV[T any](ctx context.Context, db *sql.DB, key string) (value T, err error) {
	var raw string
	err = db.QueryRowContext(ctx, `SELECT value FROM kv WHERE "key" = $1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && raw == "") {
		return value, nil
	} else if err != nil {
		return value, fmt.Errorf("failed to load '%s' from the database: %w", key, err)
	}

	err = json.Unmarshal([]byte(raw), &value)
	if err != nil {
		return value, fmt.Errorf("failed to parse '%s': %w", key, err)
	}
	return value, nil
}

// encryptSecret encrypts a value the way v3 stores the secrets of the app config
// The key derivation, the cipher and the associated data must match Pocket ID's, or v3 cannot decrypt the value
func encryptSecret(encryptionKey []byte, plaintext string) (string, error) {
	key, err := hkdf.Key(sha256.New, encryptionKey, nil, "pocketid/encrypted_string", 32)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	// The random nonce is stored in front of the ciphertext
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	ciphertext := aead.Seal(nonce, nonce, []byte(plaintext), []byte("encrypted_string"))
	return encryptedSecretPrefix + base64.StdEncoding.EncodeToString(ciphertext), nil
}
