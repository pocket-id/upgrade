// Command pocket-id-upgrade brings the database, the stored files and the actor state of an earlier Pocket ID into the form the major version that pins this release starts from
// Releases v3.x bring any Pocket ID v1 or v2 to v3
// Pocket ID runs it by itself when it finds such a database, and it can be run by hand with the same environment
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/pocket-id/upgrade/internal/install"
	"github.com/pocket-id/upgrade/internal/tov3"
)

// The upgrade to each major version, oldest first
// Each does nothing for an installation that already reached its version, so an installation of any earlier version ends up on the last one
var upgrades = []func(ctx context.Context, inst *install.Installation) error{
	tov3.Upgrade,
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := run(ctx)
	if err != nil {
		slog.Error("Failed to upgrade Pocket ID", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := install.LoadConfig()
	if err != nil {
		return err
	}
	inst, err := install.Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to connect to the database: %w", err)
	}
	defer inst.Close()

	for _, upgrade := range upgrades {
		err = upgrade(ctx, inst)
		if err != nil {
			return err
		}
	}
	slog.Info("The database is up to date")
	return nil
}
