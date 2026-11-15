# Pocket ID upgrade

Brings the database, the stored files and the actor state of an earlier Pocket ID into the form a new major version starts from.

You normally never run this yourself.
Pocket ID downloads the gzipped release of this program it was built against when it finds a database of an earlier major version, checks the download against the SHA-256 checksum compiled into Pocket ID, and decompresses and runs it once before it starts.

| Release | Upgrades                     |
| ------- | ---------------------------- |
| v3.x    | any Pocket ID v1 or v2 to v3 |

## Without internet access

If Pocket ID can't reach GitHub, it stops with an error that names the file to download and where to put it.
Download that file on another machine and put it next to the Pocket ID binary, keeping its name and without decompressing it, then start Pocket ID again.
Pocket ID checks the file against the same checksum as a download and refuses to run any other version.

With Docker, mount the file next to the binary in the container:

```sh
docker run -v ./pocket-id-upgrade_linux_amd64.gz:/app/pocket-id-upgrade_linux_amd64.gz:ro ...
```

The file is only needed for the first start, and can be removed afterwards.

## Running it by hand

Stop Pocket ID, take a backup, then decompress the binary and run it in Pocket ID's working directory with the same environment variables:

```sh
gunzip pocket-id-upgrade_linux_amd64.gz
chmod +x pocket-id-upgrade_linux_amd64
./pocket-id-upgrade_linux_amd64
```

With Docker, mount the binary into a container that uses the same volumes and environment as Pocket ID and run it there.
Pocket ID loads a `.env` file by itself, but this program doesn't, so export those variables first when you run it by hand.

## Development

```sh
go test ./...
# The Postgres tests need a server, and each test creates its own database
POCKET_ID_TEST_POSTGRES_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' go test ./...
```

`internal/install` connects to the installation the way Pocket ID does, and `internal/tov3` holds everything that brings it to v3.
`internal/tov3/migrations/` holds Pocket ID's golang-migrate files, which only change to bring data of v2 in line with v3.
`internal/tov3/testdata/baseline/` is a copy of the goose baseline of v3, which the tests compare against the migrated schema.

## Adding a major version

1. Create `internal/tov4` with an `Upgrade` function that brings an installation from the previous major version to v4, and does nothing for one that is already there.
2. Append `tov4.Upgrade` to `upgrades` in `main.go`, so an installation of any earlier version goes through every upgrade in order.
3. Release it as `v4.0.0`, and let Pocket ID v4 run it when the database is below v4's baseline.

`scripts/test-upgrades.sh` upgrades real installations of earlier releases with this tool and checks them on a build of the new major version:

```sh
# In Pocket ID: docker build -t pocket-id:upgrade-test -f docker/Dockerfile .
scripts/test-upgrades.sh pocket-id:upgrade-test
# Single cases are <release>:<sqlite|postgres>:<filesystem|database|s3>
scripts/test-upgrades.sh pocket-id:upgrade-test v2.19.2:postgres:s3
# After a release, let Pocket ID download the binary it pins instead of running a local build
TOOL=download scripts/test-upgrades.sh pocket-id:upgrade-test
```

## Releases

A release is a tag, and Pocket ID then needs the new version and the checksums from `checksums.txt` in `backend/internal/bootstrap/upgrade_tool_bootstrap.go`.
Pocket ID binaries download the exact release they pin for as long as they are in use, so a published release is never deleted or replaced.
