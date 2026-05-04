#!/usr/bin/env bash
set -euo pipefail

repo_url="https://github.com/chibbez-glitch/grafana-ad-syncher.git"
target_dir="/docker/grafana-ad-syncher"
env_file="$target_dir/deploy/.env"
tmp_env="/tmp/grafana-sync.env"

if [[ ! -f "$env_file" ]]; then
  echo "Missing .env file: $env_file" >&2
  echo "Create it from deploy/.env.example and fill in the credentials." >&2
  exit 1
fi
cp "$env_file" "$tmp_env"

if [[ ! -d "$target_dir" ]]; then
  echo "Missing target dir: $target_dir" >&2
  exit 1
fi
cd "$target_dir"
if [[ "$(pwd -P)" != "$target_dir" || "$target_dir" == "/" ]]; then
  echo "Refusing to delete outside target dir: $(pwd -P)" >&2
  exit 1
fi

rm -rf -- "$target_dir"/* "$target_dir"/.[!.]* "$target_dir"/..?*

git clone "$repo_url" .
go mod tidy
cp "$tmp_env" deploy/.env
cd deploy
docker compose up -d --build
