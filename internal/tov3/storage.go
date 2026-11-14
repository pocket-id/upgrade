package tov3

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/pocket-id/upgrade/internal/storage"
)

const (
	applicationAssetsPath       = "application-assets"
	legacyApplicationAssetsPath = "application-images"
	legacyInitedPath            = applicationAssetsPath + "/.inited"
	deletedBackgroundPath       = applicationAssetsPath + "/.deleted/background"
	oidcClientImagesPath        = "oidc-client-images"
)

// The default images earlier versions copied into the storage, by file name
// v3 only restores its current default images where none is stored
var legacyBundledImageHashes = map[string][]string{
	"logoLight.svg":  {"6d42c88cf6668f7e57c4f2a505e71ecc8a1e0a27534632aa6adec87b812d0bb0"},
	"logoDark.svg":   {"0421a8d93714bacf54c78430f1db378fd0d29565f6de59b6a89090d44a82eb16"},
	"background.jpg": {"138d510030ed845d1d74de34658acabff562d306476454369a60ab8ade31933f"},
	"background.webp": {
		"3fc436a66d6b872b01d96a4e75046c46b5c3e2daccd51e98ecdf98fd445599ab",
		"fc9ea4a7d9ac36321d9d5a34e7f0c346f5cb47254e1ee8fb75aac8b58db04230",
	},
}

// upgradeStorage moves the stored files to the layout v3 expects
func upgradeStorage(ctx context.Context, db *sql.DB, files storage.FileStorage) error {
	err := moveApplicationAssets(ctx, files)
	if err != nil {
		return fmt.Errorf("failed to move application assets: %w", err)
	}
	err = cleanApplicationAssets(ctx, files)
	if err != nil {
		return fmt.Errorf("failed to clean up application assets: %w", err)
	}
	err = moveOidcClientImages(ctx, db, files)
	if err != nil {
		return fmt.Errorf("failed to move OIDC client logos: %w", err)
	}
	return nil
}

// moveApplicationAssets moves the folder of earlier versions to its new name
// Copying everything before deleting the old folder lets an interrupted move finish on the next run
func moveApplicationAssets(ctx context.Context, files storage.FileStorage) error {
	paths, err := files.List(ctx, legacyApplicationAssetsPath)
	if err != nil || len(paths) == 0 {
		return err
	}

	for _, oldPath := range paths {
		newPath := path.Join(applicationAssetsPath, strings.TrimPrefix(oldPath, legacyApplicationAssetsPath+"/"))
		err = copyFile(ctx, files, oldPath, newPath)
		if err != nil {
			return err
		}
	}

	slog.Info("Moved application assets to their new folder", slog.Int("count", len(paths)))
	return files.DeleteAll(ctx, legacyApplicationAssetsPath)
}

// cleanApplicationAssets removes the default images of earlier versions and replaces the marker that said they were copied
func cleanApplicationAssets(ctx context.Context, files storage.FileStorage) error {
	paths, err := files.List(ctx, applicationAssetsPath)
	if err != nil {
		return err
	}

	// Delete the default images of earlier versions, so v3 restores its current ones
	inited, hasBackground := false, false
	for _, p := range paths {
		dir, name := path.Split(p)
		switch {
		case p == legacyInitedPath:
			inited = true
			continue
		case dir != applicationAssetsPath+"/":
			// The markers of deleted images live in a subfolder
			continue
		case strings.TrimSuffix(name, path.Ext(name)) == "background":
			hasBackground = true
		}

		hash, err := hashFile(ctx, files, p)
		if err != nil {
			return fmt.Errorf("failed to hash '%s': %w", p, err)
		}
		if !slices.Contains(legacyBundledImageHashes[name], hash) {
			continue
		}
		slog.Info("Removing the default application image of an earlier version", slog.String("name", name))
		err = files.Delete(ctx, p)
		if err != nil {
			return fmt.Errorf("failed to remove '%s': %w", p, err)
		}
	}

	// Earlier versions wrote one marker once the default images were copied, so a missing background meant an administrator deleted it
	// v3 tracks that per image, and would restore the background without its marker
	if !inited {
		return nil
	}
	if !hasBackground {
		err = files.Save(ctx, deletedBackgroundPath, strings.NewReader(""))
		if err != nil {
			return fmt.Errorf("failed to store the marker of the deleted background: %w", err)
		}
	}
	return files.Delete(ctx, legacyInitedPath)
}

// moveOidcClientImages moves client logos, which earlier versions stored under the raw client ID, to their hashed path
// Each logo is copied before it is deleted, so an interrupted move finishes on the next run
func moveOidcClientImages(ctx context.Context, db *sql.DB, files storage.FileStorage) error {
	paths, err := files.List(ctx, oidcClientImagesPath)
	if err != nil || len(paths) == 0 {
		return err
	}

	rows, err := db.QueryContext(ctx, `SELECT id, image_type, dark_image_type FROM oidc_clients WHERE image_type IS NOT NULL OR dark_image_type IS NOT NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()

	// Collect the moves before touching the storage, which may live in this database and need the connection the open query holds
	moves := map[string]string{}
	for rows.Next() {
		var (
			clientID      string
			imageType     sql.NullString
			darkImageType sql.NullString
		)
		err = rows.Scan(&clientID, &imageType, &darkImageType)
		if err != nil {
			return err
		}

		// Earlier versions only allowed client IDs that are safe as file names, except for CIMD clients whose URL was hashed
		legacyID := clientID
		u, err := url.Parse(clientID)
		if err == nil && strings.EqualFold(u.Scheme, "https") && u.Hostname() != "" {
			legacyID = "cimd-" + sha256Hex(clientID)
		}

		for suffix, ext := range map[string]sql.NullString{"": imageType, "-dark": darkImageType} {
			oldPath := path.Join(oidcClientImagesPath, legacyID+suffix+"."+ext.String)
			if ext.Valid && slices.Contains(paths, oldPath) {
				moves[oldPath] = path.Join(oidcClientImagesPath, sha256Hex(clientID)+suffix+"."+ext.String)
			}
		}
	}
	err = rows.Close()
	if err != nil {
		return err
	}

	for oldPath, newPath := range moves {
		err = copyFile(ctx, files, oldPath, newPath)
		if err != nil {
			return err
		}
		err = files.Delete(ctx, oldPath)
		if err != nil {
			return err
		}
	}

	if len(moves) > 0 {
		slog.Info("Moved OIDC client logos to their hashed paths", slog.Int("count", len(moves)))
	}
	return nil
}

func copyFile(ctx context.Context, files storage.FileStorage, oldPath string, newPath string) error {
	reader, err := files.Open(ctx, oldPath)
	if err != nil {
		return err
	}
	defer reader.Close()
	return files.Save(ctx, newPath, reader)
}

func hashFile(ctx context.Context, files storage.FileStorage, filePath string) (string, error) {
	reader, err := files.Open(ctx, filePath)
	if err != nil {
		return "", err
	}
	defer reader.Close()

	h := sha256.New()
	_, err = io.Copy(h, reader)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
