#!/usr/bin/env bash
# Upgrades real installations of earlier Pocket ID releases with this tool and checks them on a build of the new major version
# Usage: scripts/test-upgrades.sh <target image> [<release>:<sqlite|postgres>:<filesystem|database|s3> ...]
# TOOL=download skips the manual run, so the target downloads the release it pins
# Needs docker, go, curl, jq and sqlite3
set -euo pipefail

target=${1:?usage: $0 <target image> [<release>:<sqlite|postgres>:<filesystem|database|s3> ...]}
shift
cases=("$@")
if [ ${#cases[@]} -eq 0 ]; then
	cases=(v1.16.0:sqlite:filesystem v2.9.0:sqlite:filesystem v2.16.0:sqlite:filesystem v2.19.2:sqlite:filesystem v2.9.0:postgres:database v2.19.2:sqlite:s3)
fi

port=${PORT:-1550}
base="http://localhost:$port"
key=static-api-key-for-upgrade-test
net=pocket-id-upgrade-test
logo='<svg xmlns="http://www.w3.org/2000/svg" width="8" height="8"><rect width="8" height="8" fill="#c00"/></svg>'
work=$(mktemp -d)
failures=0

# Remove everything this script started, also when a step fails
cleanup() {
	docker rm -f pocket-id-upgrade-old pocket-id-upgrade-new pocket-id-upgrade-pg pocket-id-upgrade-s3 >/dev/null 2>&1 || true
	docker network rm "$net" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

# Runs a command until it succeeds, for at most a minute
retry() {
	for _ in $(seq 60); do
		"$@" >/dev/null 2>&1 && return 0
		sleep 1
	done
	return 1
}

# Reports whether a check passed, with the end of its output when it didn't, without stopping the remaining checks
check() {
	local name=$1 out
	shift
	if out=$("$@" 2>&1); then
		echo "  ok    $name"
	else
		echo "  FAIL  $name"
		tail -n 5 <<<"$out" | sed 's/^/        /'
		failures=$((failures + 1))
	fi
}

# Calls the API of the running instance as an admin
api() {
	local method=$1 path=$2
	shift 2
	curl -sS --fail-with-body -X "$method" "${auth[@]}" "$@" "$base$path"
}

# Runs a query against the database of the current case and prints the result
# On SQLite the second argument names the database file, which is pocket-id.db unless it's the actor data in francis.db
# The file isn't opened read-only, since a cleanly closed WAL database has no shared-memory file, and only a writable connection can create it
sql() {
	if [ "$db" = postgres ]; then
		docker exec pocket-id-upgrade-pg psql -U postgres -d "$dbname" -Atc "$1"
	else
		sqlite3 "$data/${2:-pocket-id.db}" "$1"
	fi
}

table_exists() {
	if [ "$db" = postgres ]; then
		[ "$(sql "SELECT COUNT(*) FROM information_schema.tables WHERE table_name = '$1'")" = 1 ]
	else
		[ "$(sql "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = '$1'")" = 1 ]
	fi
}

# The SMTP password must be stored encrypted in the actor state, and nowhere in plain text
smtp_password_encrypted() {
	if [ "$db" = postgres ]; then
		[ "$(sql "SELECT COUNT(*) FROM francis_actor_state WHERE actor_type = 'AppConfig' AND position('encrypted:'::bytea IN actor_state_data) > 0 AND position('smtp-plain-secret'::bytea IN actor_state_data) = 0")" = 1 ]
	else
		[ "$(sql "SELECT COUNT(*) FROM francis_actor_state WHERE actor_type = 'AppConfig' AND instr(actor_state_data, CAST('encrypted:' AS BLOB)) > 0 AND instr(actor_state_data, CAST('smtp-plain-secret' AS BLOB)) = 0" francis.db)" = 1 ]
	fi
}

config_has() {
	curl -sf -H "X-API-KEY: $key" "$base/api/admin/application-configuration" | jq -e --arg k "$1" --arg v "$2" 'map(select(.key == $k and .value == $v)) | length == 1'
}

# v3 lists a signup token by its prefix only, since it stores a hash of the token
signup_link_works() {
	curl -sS --fail-with-body -H "X-API-KEY: $key" "$base/api/admin/signup-tokens" | jq -e --arg t "$token" '(.data // .) | map(select(.prefix as $p | $p != "" and ($t | startswith($p)))) | length == 1' &&
		curl -sS --fail-with-body -X POST -H 'Content-Type: application/json' -d "{\"token\":\"$token\",\"username\":\"bob\",\"email\":\"bob@example.com\",\"firstName\":\"Bob\",\"lastName\":\"Doe\",\"displayName\":\"Bob Doe\"}" "$base/api/signup"
}

serves_logo() {
	local body
	body=$(curl -sS "$base$1")
	[ "$body" = "$logo" ] || { echo "$body"; return 1; }
}

sign_in_works() {
	local code
	code=$(docker exec pocket-id-upgrade-new /app/pocket-id one-time-access-token alice | sed -n 's|.*/lc/||p')
	curl -sf -c "$work/session" -o /dev/null -X POST -H 'Content-Type: application/json' -d "{\"token\":\"$code\"}" "$base/api/one-time-access-token" &&
		curl -sf -b "$work/session" "$base/api/users/me" | jq -e '.username == "alice"'
}

# A restart must not download the tool again, and only TOOL=download lets the first start download it
downloads_once_at_most() {
	local expected=0
	[ "${TOOL:-}" = download ] && expected=1
	docker restart pocket-id-upgrade-new &&
		retry curl -sf "$base/healthz" &&
		[ "$(docker logs pocket-id-upgrade-new 2>&1 | grep -c 'downloading the tool that upgrades it')" = "$expected" ]
}

# Build the tool for the platform the containers run on
if [ "${TOOL:-}" != download ]; then
	(cd "$(dirname "$0")/.." && CGO_ENABLED=0 GOOS=linux GOARCH="$(docker version --format '{{.Server.Arch}}')" go build -trimpath -o "$work/pocket-id-upgrade" .)
fi
printf '%s' "$logo" >"$work/logo.svg"
docker network create "$net" >/dev/null

for i in "${!cases[@]}"; do
	IFS=: read -r release db files <<<"${cases[$i]}"
	echo "== ${cases[$i]}"
	data="$work/$i/data"
	mkdir -p "$data"
	dbname="case$i"
	env_args=(-e APP_URL="$base" -e ENCRYPTION_KEY=0123456789abcdef0123456789abcdef -e STATIC_API_KEY="$key" -e ANALYTICS_DISABLED=true -e FIDO_MDS_URL=disabled -e ICON_LIBRARY_URL=disabled)
	run_args=(--network "$net" --user "$(id -u):$(id -g)" -v "$data:/app/data")

	# Give the case its own Postgres database or S3 bucket, starting the server on first use
	if [ "$db" = postgres ]; then
		docker inspect pocket-id-upgrade-pg >/dev/null 2>&1 || docker run -d --name pocket-id-upgrade-pg --network "$net" -e POSTGRES_PASSWORD=postgres postgres:17 >/dev/null
		retry docker exec pocket-id-upgrade-pg pg_isready -h localhost -U postgres
		docker exec pocket-id-upgrade-pg psql -U postgres -qc "CREATE DATABASE $dbname"
		env_args+=(-e DB_PROVIDER=postgres -e DB_CONNECTION_STRING="postgres://postgres:postgres@pocket-id-upgrade-pg:5432/$dbname?sslmode=disable")
	fi
	if [ "$files" = s3 ]; then
		docker inspect pocket-id-upgrade-s3 >/dev/null 2>&1 || docker run -d --name pocket-id-upgrade-s3 --network "$net" chrislusf/seaweedfs:4.48 server -s3 >/dev/null
		retry sh -c "echo 's3.bucket.create -name $dbname' | docker exec -i pocket-id-upgrade-s3 weed shell | grep -q 'created bucket'"
		env_args+=(-e FILE_BACKEND=s3 -e S3_BUCKET="$dbname" -e S3_REGION=us-east-1 -e S3_ENDPOINT=http://pocket-id-upgrade-s3:8333 -e S3_ACCESS_KEY_ID=any -e S3_SECRET_ACCESS_KEY=any -e S3_FORCE_PATH_STYLE=true)
	elif [ "$files" = database ]; then
		env_args+=(-e FILE_BACKEND=database)
	fi

	# Start the earlier release, which only answers on its public API once it is ready
	docker run -d --name pocket-id-upgrade-old -p "$port:1411" "${run_args[@]}" "${env_args[@]}" "ghcr.io/pocket-id/pocket-id:$release" >/dev/null
	retry curl -sf "$base/api/application-configuration" || { docker logs pocket-id-upgrade-old 2>&1 | tail -n 20; echo "$release did not start"; exit 1; }

	# Releases without a static API key get their admin session from the initial setup instead
	auth=(-H "X-API-KEY: $key")
	if ! api GET /api/application-configuration/all >/dev/null 2>&1; then
		curl -sS --fail-with-body -c "$work/cookies" -o /dev/null -X POST -H 'Content-Type: application/json' -d '{"username":"admin","email":"admin@example.com","firstName":"Ad","lastName":"Min"}' "$base/api/signup/setup"
		auth=(-b "$work/cookies")
	fi

	# Set the config the upgrade has to carry over, using whichever email settings the release has
	config=$(api GET /api/application-configuration/all | jq 'map({(.key): .value}) | add')
	notification=$(jq -r 'if has("emailLoginNotificationMode") then "always" elif has("emailLoginNotificationEnabled") then "browserRecognition" else "disabled" end' <<<"$config")
	verification=$(jq -r 'if has("emailVerification") or has("emailVerificationEnabled") then "optional" else "disabled" end' <<<"$config")
	config=$(jq '. + {appName: "Upgrade Test", smtpHost: "smtp.example.com", smtpPort: "587", smtpFrom: "noreply@example.com", smtpUser: "mailer", smtpPassword: "smtp-plain-secret", allowUserSignups: "withToken"}
		| if has("emailLoginNotificationMode") then .emailLoginNotificationMode = "always" elif has("emailLoginNotificationEnabled") then .emailLoginNotificationEnabled = "true" else . end
		| if has("emailVerification") then .emailVerification = "optional" elif has("emailVerificationEnabled") then .emailVerificationEnabled = "true" else . end' <<<"$config")
	api PUT /api/application-configuration -o /dev/null -H 'Content-Type: application/json' -d "$config"

	# Create a user to sign in with, a signup link, a client with a logo and a custom application logo
	api POST /api/users -o /dev/null -H 'Content-Type: application/json' -d '{"username":"alice","email":"alice@example.com","firstName":"Alice","lastName":"Doe","displayName":"Alice Doe","isAdmin":true}'
	token=$(api POST /api/signup-tokens -H 'Content-Type: application/json' -d '{"ttl":"24h","usageLimit":3,"userGroupIds":[]}' | jq -r .token)
	client=$(api POST /api/oidc/clients -H 'Content-Type: application/json' -d '{"name":"Upgrade App","callbackURLs":["https://app.example.com/callback"],"logoutCallbackURLs":[],"isPublic":false,"pkceEnabled":false,"credentials":{"federatedIdentities":[]}}' | jq -r .id)
	api POST "/api/oidc/clients/$client/logo" -o /dev/null -F "file=@$work/logo.svg;type=image/svg+xml"
	api PUT "/api/application-images/logo?light=true" -o /dev/null -F "file=@$work/logo.svg;type=image/svg+xml"
	docker stop pocket-id-upgrade-old >/dev/null
	docker rm pocket-id-upgrade-old >/dev/null

	# Upgrade by hand, the way an installation without access to GitHub does
	if [ "${TOOL:-}" != download ]; then
		docker run --rm "${run_args[@]}" "${env_args[@]}" -v "$work/pocket-id-upgrade:/pocket-id-upgrade:ro" --entrypoint /pocket-id-upgrade "$target" >"$work/$i/upgrade.log" 2>&1 ||
			{ echo "  FAIL  upgrade tool"; tail -n 20 "$work/$i/upgrade.log"; failures=$((failures + 1)); continue; }
	fi

	# Start the new major version and check what the upgrade had to carry over
	docker run -d --name pocket-id-upgrade-new -p "$port:1411" "${run_args[@]}" "${env_args[@]}" "$target" >/dev/null
	if ! retry curl -sf "$base/healthz"; then
		echo "  FAIL  start"
		docker logs pocket-id-upgrade-new 2>&1 | tail -n 20
		failures=$((failures + 1))
		docker rm -f pocket-id-upgrade-new >/dev/null
		continue
	fi
	echo "  ok    start"
	before=$failures
	check "app config kept" config_has appName "Upgrade Test"
	check "SMTP settings kept" config_has smtpHost smtp.example.com
	check "login notification mode is $notification" config_has emailLoginNotificationMode "$notification"
	check "email verification is $verification" config_has emailVerification "$verification"
	check "signup link works" signup_link_works
	check "client logo served" serves_logo "/api/oidc/clients/$client/logo"
	check "custom application logo served" serves_logo "/api/application-assets/logo?light=true"
	check "default background served" curl -sf -o /dev/null "$base/api/application-assets/background"
	check "sign-in works" sign_in_works
	check "restart downloads nothing" downloads_once_at_most

	# Read the database only once Pocket ID has closed it, since SQLite's locks don't reach across the file sharing of Docker Desktop and similar
	docker stop pocket-id-upgrade-new >/dev/null
	check "golang-migrate table removed" eval '! table_exists schema_migrations'
	check "goose table created" table_exists goose_db_version
	check "SMTP password encrypted" smtp_password_encrypted
	if [ "$db" = sqlite ]; then
		check "actor data moved out of pocket-id.db" eval '! table_exists francis_actor_state'
	fi
	if [ "$failures" -gt "$before" ]; then
		docker logs pocket-id-upgrade-new 2>&1 | grep -E ' (ERR|WRN) ' | grep -v 'networked file system\|MAXMIND' | tail -n 20 | sed 's/^/        /'
	fi
	docker rm -f pocket-id-upgrade-new >/dev/null
done

if [ "$failures" -gt 0 ]; then
	echo "$failures checks failed"
	exit 1
fi
echo "All checks passed"
