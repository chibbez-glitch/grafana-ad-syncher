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
All settings are baked into [`deploy/docker-compose.yml`](deploy/docker-compose.yml).
Edit the file in the git repo, commit, push, then redeploy. The two
`REPLACE_ME_*` placeholders (Grafana admin password and Entra client secret)
must be replaced before the first deploy — `deploy.sh` aborts otherwise.

Recognised env vars (set on the `grafana-sync` container in the compose file):

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
- `ALLOW_REMOVE_TEAM_MEMBERS` (`true`/`false`)
- `DATA_DIR` (default `/data`)
- `LISTEN_ADDR` (default `:8080`)

## Usage
1. Run `bash deploy.sh` on the target host. It wipes `/docker/grafana-ad-syncher`,
   re-clones the repo and runs `docker compose up -d --build`.
2. Open the UI at `http://<host>:8080`.
3. Create a Grafana Org entry using the **Grafana Org ID** from Grafana.
4. Add mappings of Entra Group IDs to Grafana Team names.
5. Click **Preview sync** to review planned actions, then **Apply plan**.

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
