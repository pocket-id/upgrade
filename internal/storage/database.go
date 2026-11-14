package storage

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"
)

type databaseStorage struct {
	db       *sql.DB
	postgres bool
}

// NewDatabaseStorage creates a storage provider that keeps the files in the "storage" table
func NewDatabaseStorage(db *sql.DB, postgres bool) FileStorage {
	return &databaseStorage{db: db, postgres: postgres}
}

func (s *databaseStorage) Save(ctx context.Context, relativePath string, data io.Reader) error {
	b, err := io.ReadAll(data)
	if err != nil {
		return fmt.Errorf("failed to read data: %w", err)
	}

	// Pocket ID stores timestamps as Unix seconds in SQLite and as timestamps in Postgres
	var now any = time.Now().Unix()
	if s.postgres {
		now = time.Now()
	}

	_, err = s.db.ExecContext(ctx, `INSERT INTO storage (path, data, size, mod_time, created_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (path) DO UPDATE SET data = excluded.data, size = excluded.size, mod_time = excluded.mod_time`,
		relativePath, b, len(b), now, now)
	if err != nil {
		return fmt.Errorf("failed to save file to database: %w", err)
	}
	return nil
}

func (s *databaseStorage) Open(ctx context.Context, relativePath string) (io.ReadCloser, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM storage WHERE path = $1`, relativePath).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fs.ErrNotExist
	} else if err != nil {
		return nil, fmt.Errorf("failed to read file from database: %w", err)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *databaseStorage) Delete(ctx context.Context, relativePath string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM storage WHERE path = $1`, relativePath)
	if err != nil {
		return fmt.Errorf("failed to delete file from database: %w", err)
	}
	return nil
}

func (s *databaseStorage) DeleteAll(ctx context.Context, prefix string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM storage WHERE path LIKE $1`, prefix+"/%")
	if err != nil {
		return fmt.Errorf("failed to delete files with prefix '%s' from database: %w", prefix, err)
	}
	return nil
}

func (s *databaseStorage) List(ctx context.Context, root string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM storage WHERE path LIKE $1`, root+"/%")
	if err != nil {
		return nil, fmt.Errorf("failed to list files from database: %w", err)
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var path string
		err = rows.Scan(&path)
		if err != nil {
			return nil, fmt.Errorf("failed to list files from database: %w", err)
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}
