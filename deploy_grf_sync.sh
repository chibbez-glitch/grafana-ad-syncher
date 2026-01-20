#!/usr/bin/env bash
set -euo pipefail

repo_url="https://github.com/chibbez-glitch/grafana-ad-syncher.git"
target_dir="/docker/grafana-ad-syncher"
source_compose="/docker/grafana-ad-syncher/grafana-ad-syncher/deploy/docker-compose.yml"
tmp_compose="/tmp/docker-compose.yml"

if [[ ! -f "$source_compose" ]]; then
  echo "Missing source compose: $source_compose" >&2
  exit 1
fi
cp "$source_compose" "$tmp_compose"

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
cp "$tmp_compose" deploy/docker-compose.yml
cd deploy
docker compose up -d --build
