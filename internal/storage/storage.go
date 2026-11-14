// Package storage is a trimmed copy of Pocket ID's file storage backends, with only the operations the upgrade needs
package storage

import (
	"context"
	"io"
)

type FileStorage interface {
	Save(ctx context.Context, relativePath string, data io.Reader) error
	// Open returns an error matching fs.ErrNotExist when the file doesn't exist
	Open(ctx context.Context, relativePath string) (io.ReadCloser, error)
	Delete(ctx context.Context, relativePath string) error
	DeleteAll(ctx context.Context, prefix string) error
	// List returns the path of every file below root, including the ones in nested folders
	List(ctx context.Context, root string) ([]string, error)
}
