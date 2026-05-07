#!/usr/bin/env bash
set -euo pipefail

repo_url="https://github.com/chibbez-glitch/grafana-ad-syncher.git"
target_dir="/docker/grafana-ad-syncher"

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

# Fail fast if secret placeholders haven't been replaced.
compose_file="deploy/docker-compose.yml"
if grep -q "REPLACE_ME_" "$compose_file"; then
  echo "ERROR: Secret placeholders still in $compose_file (REPLACE_ME_*)." >&2
  echo "       Edit the file in the git repository, commit, push, then re-run deploy.sh." >&2
  exit 1
fi

go mod tidy
cd deploy
docker compose up -d --build
