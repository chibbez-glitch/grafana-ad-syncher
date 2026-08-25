#!/usr/bin/env bash
set -euo pipefail

repo_url="https://github.com/chibbez-glitch/grafana-ad-syncher.git"
target_dir="/docker/grafana-ad-syncher"

# The .env lives next to docker-compose.yml, at $target_dir/deploy/.env. That is
# where docker compose resolves ${VAR} from and where you would expect to find
# it.
#
# The reason it used to live in /etc instead: this script wipes the whole
# checkout on every run, so anything inside it was destroyed. That is worked
# around here by stashing the file across the wipe rather than by moving it out
# of the way. It is never committed - .gitignore covers deploy/.env.
env_file="$target_dir/deploy/.env"
legacy_env="/etc/grafana-ad-syncher.env"

stash="$(mktemp)"
trap 'rm -f "$stash"' EXIT

have_env=0
if [[ -f "$env_file" ]]; then
  cp "$env_file" "$stash"
  have_env=1
elif [[ -f "$legacy_env" ]]; then
  # One-time migration off the old /etc location, so nobody has to retype
  # secrets. The /etc copy is not read again after this.
  cp "$legacy_env" "$stash"
  have_env=1
  echo "Adopting $legacy_env into deploy/.env. The /etc copy is no longer used;"
  echo "delete it once this deploy succeeded:  rm $legacy_env"
fi

mkdir -p "$target_dir"
cd "$target_dir"
if [[ "$(pwd -P)" != "$target_dir" || "$target_dir" == "/" ]]; then
  echo "Refusing to delete outside target dir: $(pwd -P)" >&2
  exit 1
fi

# Wipe and re-clone so we always run with a clean repo state.
rm -rf -- "$target_dir"/* "$target_dir"/.[!.]* "$target_dir"/..?* 2>/dev/null || true
git clone "$repo_url" .

if [[ "$have_env" -eq 1 ]]; then
  install -m 600 "$stash" deploy/.env
else
  # First run on this host: seed the file from the template and stop, so the
  # secrets get filled in before anything is started.
  install -m 600 deploy/env.example deploy/.env
  echo "" >&2
  echo "Created $env_file from the template." >&2
  echo "Fill in the values, then re-run deploy.sh:" >&2
  echo "" >&2
  echo "    vi $env_file" >&2
  echo "" >&2
  exit 1
fi

if grep -q "REPLACE_ME_" deploy/.env; then
  echo "ERROR: placeholders still in $env_file (REPLACE_ME_*)." >&2
  echo "       Fill in the real values, then re-run deploy.sh." >&2
  exit 1
fi

# Strip CR line endings. compose reads this file with `env_file:`, where a
# trailing \r becomes part of the *value* - GRAFANA_URL would end up as
# "https://grafana:443\r" and fail with an error that points nowhere near here.
# Easy to introduce by pasting from a Windows clipboard, so normalise rather
# than trust.
sed -i 's/\r$//' deploy/.env

# Reading the bind-mounted docker socket needs the gid that owns it on *this*
# host, because the container runs as a non-root user. Detect it instead of
# hardcoding a value that differs per distribution. Any previous line is
# removed first, otherwise repeated deploys keep appending.
sed -i '/^DOCKER_GID=/d' deploy/.env
if [[ -S /var/run/docker.sock ]]; then
  docker_gid="$(stat -c '%g' /var/run/docker.sock)"
  printf 'DOCKER_GID=%s\n' "$docker_gid" >> deploy/.env
  echo "docker socket owned by gid $docker_gid; container joins that group"
else
  echo "WARNING: /var/run/docker.sock not found - /api/logs/docker will be unavailable." >&2
fi

# API_TOKEN guards /api/logs/*. An empty value is fine - the app then generates
# a token on first start and prints it once to the container log. But compose
# warns about undefined variables, so make sure the key at least exists.
if ! grep -q '^API_TOKEN=' deploy/.env; then
  printf 'API_TOKEN=\n' >> deploy/.env
fi

go mod tidy
cd deploy
docker compose up -d --build
