#!/usr/bin/env bash
set -euo pipefail

repo_url="https://github.com/chibbez-glitch/grafana-ad-syncher.git"
target_dir="/docker/grafana-ad-syncher"

# Credentials live outside target_dir on purpose. This script wipes the whole
# checkout on every run, so a .env kept inside it was destroyed each time — which
# is why the secrets ended up hardcoded in docker-compose.yml instead.
env_source="/etc/grafana-ad-syncher.env"

if [[ ! -f "$env_source" ]]; then
  echo "ERROR: $env_source not found." >&2
  echo "       Create it from deploy/env.example in the repository:" >&2
  echo "         install -m 600 /dev/null $env_source" >&2
  echo "         vi $env_source" >&2
  exit 1
fi

if grep -q "REPLACE_ME_" "$env_source"; then
  echo "ERROR: Secret placeholders still in $env_source (REPLACE_ME_*)." >&2
  echo "       Fill in the real values, then re-run deploy.sh." >&2
  exit 1
fi

if [[ ! -d "$target_dir" ]]; then
  mkdir -p "$target_dir"
fi

cd "$target_dir"
if [[ "$(pwd -P)" != "$target_dir" || "$target_dir" == "/" ]]; then
  echo "Refusing to delete outside target dir: $(pwd -P)" >&2
  exit 1
fi

# Wipe and re-clone so we always run with a clean repo state.
rm -rf -- "$target_dir"/* "$target_dir"/.[!.]* "$target_dir"/..?* 2>/dev/null || true
git clone "$repo_url" .

# docker compose substitutes ${VAR} from the .env file in its project directory,
# which is deploy/. Restore it here, since the wipe above removed it.
install -m 600 "$env_source" deploy/.env

# Reading the bind-mounted docker socket needs the gid that owns it on *this*
# host, because the container runs as a non-root user. Detect it instead of
# hardcoding a value that differs per distribution. Appended last so it wins
# over any stale DOCKER_GID in $env_source.
if [[ -S /var/run/docker.sock ]]; then
  docker_gid="$(stat -c '%g' /var/run/docker.sock)"
  printf '\nDOCKER_GID=%s\n' "$docker_gid" >> deploy/.env
  echo "docker socket owned by gid $docker_gid; container joins that group"
else
  echo "WARNING: /var/run/docker.sock not found - /api/logs/docker will be unavailable." >&2
fi

# API_TOKEN guards /api/logs/*. Leaving it empty is fine: the app generates one
# on first start and prints it to the container log. But compose warns loudly
# about undefined variables, so make sure the key exists.
if ! grep -q '^API_TOKEN=' deploy/.env; then
  printf 'API_TOKEN=\n' >> deploy/.env
fi

go mod tidy
cd deploy
docker compose up -d --build
