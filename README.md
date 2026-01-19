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
Set these env vars in the container:

- `GRAFANA_URL` (e.g. `http://grafana:3000`)
- `GRAFANA_ADMIN_USER` / `GRAFANA_ADMIN_PASSWORD` (server admin)
- `GRAFANA_ADMIN_TOKEN` (optional; if set it is preferred)
- `ENTRA_TENANT_ID`
- `ENTRA_CLIENT_ID`
- `ENTRA_CLIENT_SECRET`
- `SYNC_INTERVAL` (e.g. `15m`)
- `DEFAULT_USER_ROLE` (`Viewer`, `Editor`, `Admin`)
- `ALLOW_CREATE_USERS` (`true`/`false`)
- `ALLOW_REMOVE_TEAM_MEMBERS` (`true`/`false`)
- `DATA_DIR` (default `/data`)
- `LISTEN_ADDR` (default `:8080`)
- `SYNC_INTERVAL` set to `0` to disable automatic sync

## Usage
1. Build and run the container (example compose: `deploy/docker-compose.example.yml`).
2. Open the UI at `http://<host>:8085` (or the port you mapped).
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
