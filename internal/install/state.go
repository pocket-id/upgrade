package install

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/italypaleale/francis/actor"
	"github.com/italypaleale/francis/components"
	"github.com/italypaleale/francis/components/postgres"
	"github.com/italypaleale/francis/components/sqlite"
	"github.com/italypaleale/francis/host/local"
	"github.com/italypaleale/francis/host/remote"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The deadline only needs to exceed the provider's query timeout, since nothing here registers a host
const hostHealthCheckDeadline = 90 * time.Second

// StateStore reads and writes the state of Pocket ID's actors
type StateStore interface {
	// GetState returns actor.ErrStateNotFound when the actor has no state
	GetState(ctx context.Context, actorType string, actorID string, dest any) error
	SetState(ctx context.Context, actorType string, actorID string, state any, opts *actor.SetStateOpts) error
	ListStates(ctx context.Context, actorType string, opts *actor.ListStatesOpts) (actor.StateList, error)
	DeleteState(ctx context.Context, actorType string, actorID string) error
}

// WithStateStore calls fn with access to the actor state, which lives in Pocket ID's database unless a standalone Francis runtime is configured
func (i *Installation) WithStateStore(ctx context.Context, fn func(store StateStore) error) error {
	if len(i.Config.FrancisAddresses) > 0 {
		return withRemoteStateStore(ctx, i.Config, fn)
	}

	// Only the provider's Init creates and upgrades the Francis tables, which a database from before v2.10 doesn't have at all
	provider, providerOpt, err := newActorProvider(i.DB, i.Pool)
	if err != nil {
		return fmt.Errorf("failed to create actor provider: %w", err)
	}
	err = provider.Init(ctx)
	_ = provider.Close()
	if err != nil {
		return fmt.Errorf("failed to initialize actor provider: %w", err)
	}

	// The host is never run, so it joins no cluster and binds no port, and only its direct state operations are used
	// It still requires an address and a PSK, which therefore never leave this process
	psk := make([]byte, 32)
	_, _ = rand.Read(psk)
	store, err := local.NewHost(
		local.WithAddress("127.0.0.1:1"),
		local.WithLogger(slog.Default().With("scope", "actor-state")),
		local.WithHostHealthCheckDeadline(hostHealthCheckDeadline),
		local.WithRuntimePSKs(psk),
		providerOpt,
	)
	if err != nil {
		return fmt.Errorf("failed to create actor state store: %w", err)
	}
	return fn(store)
}

// newActorProvider creates the Francis provider of the embedded runtime, and the option that makes a host use the same database
func newActorProvider(db *sql.DB, pg *pgxpool.Pool) (components.ActorProvider, local.HostOption, error) {
	providerConfig := components.NewProviderConfig()
	providerConfig.HostHealthCheckDeadline = hostHealthCheckDeadline
	log := slog.Default().With("scope", "actor-state")

	if pg != nil {
		opts := postgres.PostgresProviderOptions{DB: pg}
		provider, err := postgres.NewPostgresProvider(log, opts, providerConfig)
		return provider, local.WithPostgresProvider(opts), err
	}
	opts := sqlite.SQLiteProviderOptions{DB: db}
	provider, err := sqlite.NewSQLiteProvider(log, opts, providerConfig)
	return provider, local.WithSQLiteProvider(opts), err
}

// withRemoteStateStore joins the standalone Francis runtime as a client for the duration of fn
func withRemoteStateStore(parentCtx context.Context, cfg *Config, fn func(store StateStore) error) error {
	const connectTimeout = 30 * time.Second

	// Address and authenticate to the runtime with the same settings as Pocket ID
	opts := []remote.HostOption{
		remote.WithLogger(slog.Default().With("scope", "actor-client")),
		remote.WithRuntimeAddresses(cfg.FrancisAddresses...),
		remote.WithClientOnly(),
		remote.WithShutdownGracePeriod(2 * time.Second),
	}
	switch {
	case len(cfg.FrancisHostPSK) > 0:
		opts = append(opts, remote.WithHostBootstrapPSK(cfg.FrancisHostPSK))
	case cfg.FrancisHostJWTFile != "":
		opts = append(opts, remote.WithHostBootstrapJWTFile(cfg.FrancisHostJWTFile))
	}
	if len(cfg.FrancisCA) > 0 {
		opts = append(opts, remote.WithPinnedCA(cfg.FrancisCA))
	} else {
		opts = append(opts, remote.WithUnsafeNoPinnedCA())
	}

	client, err := remote.NewHost(opts...)
	if err != nil {
		return fmt.Errorf("failed to create the actor client: %w", err)
	}

	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- client.Run(ctx)
	}()

	// Francis reconnects to the runtime indefinitely, so an unreachable one needs a deadline here
	select {
	case <-client.Ready():
	case err = <-runErrCh:
		return fmt.Errorf("failed to connect to the Francis runtime: %w", err)
	case <-time.After(connectTimeout):
		cancel()
		<-runErrCh
		return fmt.Errorf("timed out connecting to the Francis runtime after %v", connectTimeout)
	}

	fnErr := fn(client)

	// Leave the cluster before returning, so the runtime drops the registration instead of waiting for the health check to lapse
	cancel()
	err = <-runErrCh
	switch {
	case fnErr != nil:
		return fnErr
	case err != nil && !errors.Is(err, context.Canceled):
		return fmt.Errorf("error disconnecting from the Francis runtime: %w", err)
	default:
		return nil
	}
}
