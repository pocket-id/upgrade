package install

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/caarlos0/env/v11"
)

// Config holds the settings of Pocket ID that the upgrade needs, read from the same environment variables
type Config struct {
	EncryptionKey      []byte `env:"ENCRYPTION_KEY"`
	DBConnectionString string `env:"DB_CONNECTION_STRING"`

	FileBackend                     string `env:"FILE_BACKEND"`
	UploadPath                      string `env:"UPLOAD_PATH"`
	S3Bucket                        string `env:"S3_BUCKET"`
	S3Region                        string `env:"S3_REGION"`
	S3Endpoint                      string `env:"S3_ENDPOINT"`
	S3AccessKeyID                   string `env:"S3_ACCESS_KEY_ID"`
	S3SecretAccessKey               string `env:"S3_SECRET_ACCESS_KEY"`
	S3ForcePathStyle                bool   `env:"S3_FORCE_PATH_STYLE"`
	S3DisableDefaultIntegrityChecks bool   `env:"S3_DISABLE_DEFAULT_INTEGRITY_CHECKS"`

	FrancisHost        string `env:"EXPERIMENTAL_FRANCIS_HOST"`
	FrancisHostPSK     []byte `env:"EXPERIMENTAL_FRANCIS_HOST_PSK"`
	FrancisHostJWTFile string `env:"EXPERIMENTAL_FRANCIS_HOST_JWT_FILE"`
	FrancisCA          []byte `env:"EXPERIMENTAL_FRANCIS_CA"`

	// Postgres is derived from the connection string
	Postgres bool
	// FrancisAddresses are the addresses of a standalone Francis runtime, and empty when the runtime is embedded in Pocket ID
	FrancisAddresses []string
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	err := env.ParseWithOptions(cfg, env.Options{
		FuncMap: map[reflect.Type]env.ParserFunc{
			reflect.TypeFor[[]byte](): func(value string) (any, error) {
				return []byte(value), nil
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("error parsing env config: %w", err)
	}

	// Load the values Pocket ID also accepts from the file named by the "_FILE" variant of their variable
	// Strings are trimmed like Pocket ID does, while keys are used byte for byte, because a trimmed ENCRYPTION_KEY would encrypt secrets Pocket ID cannot decrypt
	for name, dest := range map[string]any{
		"ENCRYPTION_KEY":                &cfg.EncryptionKey,
		"DB_CONNECTION_STRING":          &cfg.DBConnectionString,
		"S3_SECRET_ACCESS_KEY":          &cfg.S3SecretAccessKey,
		"EXPERIMENTAL_FRANCIS_HOST_PSK": &cfg.FrancisHostPSK,
		"EXPERIMENTAL_FRANCIS_CA":       &cfg.FrancisCA,
	} {
		fileName := os.Getenv(name + "_FILE")
		if fileName == "" {
			continue
		}
		content, err := os.ReadFile(fileName)
		if err != nil {
			return nil, fmt.Errorf("failed to read file '%s' for env var %s_FILE: %w", fileName, name, err)
		}
		switch d := dest.(type) {
		case *string:
			*d = strings.TrimSpace(string(content))
		case *[]byte:
			*d = content
		}
	}

	// A shorter key is rejected by Pocket ID, so secrets encrypted with it could never be read
	if len(cfg.EncryptionKey) < 16 {
		return nil, errors.New("ENCRYPTION_KEY must be at least 16 bytes long")
	}

	// Pocket ID uses SQLite unless the connection string is a Postgres URL, and then the value names the folder of its database files
	switch {
	case cfg.DBConnectionString == "":
		cfg.DBConnectionString = "data"
	case strings.HasPrefix(cfg.DBConnectionString, "postgres://") || strings.HasPrefix(cfg.DBConnectionString, "postgresql://"):
		cfg.Postgres = true
	}
	if !cfg.Postgres {
		err = checkSqliteFolder(cfg.DBConnectionString)
		if err != nil {
			return nil, err
		}
	}

	// The upload path only has a default for the filesystem backend, since S3 uses it as an optional key prefix
	cfg.FileBackend = strings.ToLower(cfg.FileBackend)
	switch cfg.FileBackend {
	case "s3", "database":
	case "", "filesystem":
		cfg.FileBackend = "filesystem"
		if cfg.UploadPath == "" {
			cfg.UploadPath = "data/uploads"
		}
	default:
		return nil, errors.New("invalid FILE_BACKEND value. Must be 'filesystem', 'database', or 's3'")
	}

	// Any value other than "embedded" lists the addresses of a standalone Francis runtime
	francisHost := strings.TrimSpace(strings.ToLower(cfg.FrancisHost))
	if francisHost != "" && francisHost != "embedded" {
		for address := range strings.SplitSeq(francisHost, ",") {
			address = strings.TrimSpace(address)
			if address != "" {
				cfg.FrancisAddresses = append(cfg.FrancisAddresses, address)
			}
		}
		cfg.FrancisHostPSK = bytes.TrimSpace(cfg.FrancisHostPSK)
	}

	return cfg, nil
}

// SqliteConnString returns the connection string of one SQLite database file in the folder DB_CONNECTION_STRING names, with the query parameters set there
func (c *Config) SqliteConnString(file string) string {
	dir, query, _ := strings.Cut(strings.TrimPrefix(c.DBConnectionString, "file:"), "?")
	connString := "file:" + filepath.Join(dir, file)
	if query != "" {
		connString += "?" + query
	}
	return connString
}

// checkSqliteFolder rejects a connection string that names a database file, which is what Pocket ID before v3 expected, with the same message as Pocket ID
// A path that doesn't exist yet but looks like a file is rejected too, since it would otherwise become a folder of that name
func checkSqliteFolder(connString string) error {
	dir, _, _ := strings.Cut(strings.TrimPrefix(connString, "file:"), "?")
	info, err := os.Stat(dir)
	isFile := err == nil && info.Mode().IsRegular()
	switch strings.ToLower(filepath.Ext(dir)) {
	case ".db", ".sqlite", ".sqlite3":
		isFile = true
	}
	if isFile {
		return fmt.Errorf("DB_CONNECTION_STRING must be the folder of the SQLite databases instead of a database file: set it to '%s' and make sure the database file in it is named pocket-id.db", filepath.Dir(dir))
	}
	return nil
}
