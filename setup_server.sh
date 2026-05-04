#!/usr/bin/env bash
set -euo pipefail

ENV_FILE="/docker/grafana-ad-syncher/deploy/.env"

echo ""
echo "=== Grafana AD Syncher - Server Setup ==="
echo ""

if [[ -f "$ENV_FILE" ]]; then
  echo "Eine .env Datei existiert bereits unter $ENV_FILE"
  read -rp "Ueberschreiben? (j/N): " confirm
  [[ "$confirm" == "j" || "$confirm" == "J" ]] || { echo "Abgebrochen."; exit 0; }
fi

mkdir -p "$(dirname "$ENV_FILE")"

echo "Bitte die Zugangsdaten eingeben:"
echo ""

read -rp  "Grafana Admin User:     " grafana_user
read -rsp "Grafana Admin Passwort: " grafana_password; echo
read -rp  "Grafana Admin Token (leer lassen falls Passwort genutzt wird): " grafana_token
echo ""
read -rp  "Entra Tenant ID:        " entra_tenant
read -rp  "Entra Client ID:        " entra_client
read -rsp "Entra Client Secret:    " entra_secret; echo

cat > "$ENV_FILE" << EOF
GRAFANA_ADMIN_USER=$grafana_user
GRAFANA_ADMIN_PASSWORD=$grafana_password
GRAFANA_ADMIN_TOKEN=$grafana_token
ENTRA_TENANT_ID=$entra_tenant
ENTRA_CLIENT_ID=$entra_client
ENTRA_CLIENT_SECRET=$entra_secret
EOF

chmod 600 "$ENV_FILE"

echo ""
echo "Fertig! .env wurde angelegt unter: $ENV_FILE"
echo ""
echo "Jetzt das Deploy-Script ausfuehren:"
echo "  bash /docker/grafana-ad-syncher/deploy_grf_sync.sh"
echo ""
