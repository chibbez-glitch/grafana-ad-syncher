# Grafana AD/Entra Sync Service

This service syncs Entra ID group memberships into Grafana teams, assigns org roles, and keeps team membership in sync. It runs as a third container next to Grafana + MySQL and talks to Grafana via the HTTP API (no direct DB writes).

## Features
- Entra ID group -> Grafana team mapping
- Ensures users exist in Grafana
- Adds users to orgs and updates org roles
- Removes users from teams when they leave the Entra group
- Minimal web UI for administration

## Requirements
- Grafana Community Edition (current version)
- Grafana server admin credentials (user/pass or admin token)
- Entra ID app registration with permissions:
  - `User.Read.All`
  - `Group.Read.All`

## Configuration
Everything lives in **one file**: `deploy/.env`, next to
[`deploy/docker-compose.yml`](deploy/docker-compose.yml). The compose file loads
it wholesale via `env_file:` and contains no settings and no secrets of its own —
it is tracked in a public repository, so nothing sensitive may go in it.

`deploy/.env` is never committed (`.gitignore` covers it). `deploy.sh` creates
it from [`deploy/env.example`](deploy/env.example) on the first run of a host,
and carries it across the wipe-and-re-clone on every later run.
`env.example` documents every key inline and is the file to read when in doubt.

Recognised keys:

- `GRAFANA_URL` (default `http://grafana:3000` — talks to the grafana container in the shared docker network)
- `GRAFANA_INSECURE_TLS` (`true` to skip TLS verification — only relevant if `GRAFANA_URL` is HTTPS)
- `GRAFANA_DEBUG` (`true` enables DNS/TCP/TLS/TTFB logging per request, plus startup `/etc/hosts` dump and reachability probe)
- `GRAFANA_ADMIN_USER` / `GRAFANA_ADMIN_PASSWORD` (server admin)
- `GRAFANA_ADMIN_TOKEN` (optional; if set it is preferred)
- `ENTRA_TENANT_ID`
- `ENTRA_CLIENT_ID`
- `ENTRA_CLIENT_SECRET`
- `SYNC_INTERVAL` (e.g. `15m`; `0` disables automatic sync)
- `AUTO_SYNC_ON_START` (`true`/`false`) — if set, forces the persisted auto-sync flag to this value at every container start, overriding the UI toggle. Leave unset to let the UI toggle decide.
- `DEFAULT_USER_ROLE` (`Viewer`, `Editor`, `Admin`)
- `ALLOW_CREATE_USERS` (`true`/`false`)
- `ALLOW_REMOVE_TEAM_MEMBERS` (`true`/`false`) — the only destructive action this service has; it removes a user from a *team*, never deletes the user
- `REVIEW_EXCLUDE_USERS` (default `admin`) — comma-separated logins/e-mails kept out of the “Accounts to review” panel; `GRAFANA_ADMIN_USER` is excluded automatically
- `MANAGE_ORG_ROLES` (`true`/`false`, default `true`) — set `false` when Grafana maps org roles from the OAuth token itself. It then rejects API role changes with `org.externallySynced`, so the action can never succeed and would reappear in every plan
- `DATA_DIR` (default `/data`)
- `LISTEN_ADDR` (default `:8080`)
- `DISPLAY_TIMEZONE` (default `Europe/Luxembourg`) — IANA zone for UI timestamps only; log and API stay UTC
- `API_TOKEN` — bearer token for the log API below. Empty means "generate one on first start"; there is no unauthenticated mode.
- `LOG_BUFFER_LINES` (default `5000`) — how many app log lines are kept in memory
- `DOCKER_SOCKET` (default `/var/run/docker.sock`)
- `CONTAINER_NAME` (default `grafana-sync`) — the container `/api/logs/docker` reads by default

## Log API

Troubleshooting endpoints on the same port as the UI (`8080`), all read-only
and all behind a bearer token.

Getting the token — either pin one in `deploy/.env` (`API_TOKEN=` plus the
output of `openssl rand -hex 32`), or let the app generate one on first start
and read it back once from the container log:

```bash
docker logs grafana-sync | grep "shown here once"
```

The generated token is persisted in the data volume, so it survives restarts.
It is printed in full only on the start that created it; later starts log just
the first 8 characters.

```bash
T=<token>
H="Authorization: Bearer $T"

curl -sH "$H" http://<host>:8080/api/logs                    # overview + docker socket status
curl -sH "$H" "http://<host>:8080/api/logs/app?tail=200"     # this app, from the in-memory ring buffer
curl -sH "$H" "http://<host>:8080/api/logs/containers"       # what the daemon can see
curl -sH "$H" "http://<host>:8080/api/logs/docker?container=grafana&tail=500&since=30m"
```

Parameters: `tail` (lines; `all` allowed on the docker route), `since`
(`15m`, `2h`, or an RFC3339 timestamp), `q` (case-insensitive substring),
`stream` (`stdout`/`stderr`, docker route only), `format` (`text`, the default,
or `json`). `?token=<token>` works instead of the header when a browser tab is
easier than curl.

`/api/logs/app` is a copy of what the app writes to stderr, so it works with no
extra privileges. `/api/logs/docker` needs `/var/run/docker.sock` bind-mounted
and the container joined to the socket's group — `deploy.sh` detects that gid
and writes `DOCKER_GID` into `deploy/.env`. If the mount is missing the
endpoint says so explicitly rather than failing vaguely.

> Access to the docker socket is equivalent to root on the host, and `:ro` on
> the mount does not change that — it only stops the socket file being
> replaced. `API_TOKEN` is the only thing guarding it. Remove the socket mount
> from `deploy/docker-compose.yml` if that trade is not acceptable;
> `/api/logs/app` keeps working without it.

## Usage
1. Run `./deploy.sh` on the target host. On the first run it creates
   `/docker/grafana-ad-syncher/deploy/.env` from the template and stops.
2. Fill in the secrets: `vi /docker/grafana-ad-syncher/deploy/.env`.
3. Run `./deploy.sh` again. It wipes and re-clones `/docker/grafana-ad-syncher`
   (keeping `deploy/.env`) and runs `docker compose up -d --build`.
4. Open the UI at `http://<host>:8080`.
5. Create a Grafana Org entry using the **Grafana Org ID** from Grafana.
6. Add mappings of Entra Group IDs to Grafana Team names.
7. Click **Preview sync** to review planned actions, then **Apply plan**.

## Notes
- Org Role can be set per org or per mapping (role override).
- Team IDs are stored after the first sync or when teams are created.
- This service only syncs Entra groups. LDAP/AD can be added later if needed.
- The Grafana API endpoints used are the standard Admin/Org/Team endpoints.

## Build
```bash
GOOS=linux GOARCH=amd64 go build -o syncd ./cmd/syncd
```

## Run locally
```bash
LISTEN_ADDR=":8080" \
DATA_DIR="/tmp/grafana-sync" \
GRAFANA_URL="http://localhost:3000" \
GRAFANA_ADMIN_USER="admin" \
GRAFANA_ADMIN_PASSWORD="admin" \
ENTRA_TENANT_ID="..." \
ENTRA_CLIENT_ID="..." \
ENTRA_CLIENT_SECRET="..." \
./syncd
```
