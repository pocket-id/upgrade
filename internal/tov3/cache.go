package tov3

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/pocket-id/upgrade/internal/install"
)

// legacyGeoLiteDBPath is where earlier versions kept the GeoLite database unless GEOLITE_DB_PATH named another place
const legacyGeoLiteDBPath = "data/GeoLite2-City.mmdb"

// moveGeoLiteDB moves the GeoLite database from the data folder to the cache folder, where v3 keeps it by default
// A database that was placed by hand can't be downloaded again, so it must not be left behind
func moveGeoLiteDB(cfg *install.Config) (err error) {
	// A path that was set explicitly still applies in v3
	if cfg.GeoLiteDBPath != "" {
		return nil
	}

	src, err := os.Open(legacyGeoLiteDBPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer src.Close()

	// Copy instead of renaming, because the cache folder is usually on another volume than the data folder
	// Writing to a temporary file first keeps an interrupted copy from leaving half a database where v3 would load it
	err = os.MkdirAll(cfg.CachePath, 0700)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(cfg.CachePath, "geolite.*.mmdb.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	_, err = io.Copy(tmp, src)
	if err != nil {
		return err
	}
	err = tmp.Close()
	if err != nil {
		return err
	}
	newPath := filepath.Join(cfg.CachePath, "GeoLite2-City.mmdb")
	err = os.Rename(tmp.Name(), newPath)
	if err != nil {
		return err
	}

	// Remove the old file last, so an interrupted move is simply repeated
	err = os.Remove(legacyGeoLiteDBPath)
	if err != nil {
		return fmt.Errorf("failed to remove the old file: %w", err)
	}
	slog.Info("Moved the GeoLite database to the cache folder", slog.String("path", newPath))
	return nil
}
