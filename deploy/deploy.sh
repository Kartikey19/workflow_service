#!/usr/bin/env bash
# Run on the host from the deploy directory. Compose reads its private .env.
set -euo pipefail
cd "$(dirname "$0")"
if [[ ! -f .env ]]; then echo 'Run bootstrap.sh first.' >&2; exit 1; fi
replicas=${WORKER_REPLICAS:-2}
[[ "$replicas" =~ ^[1-9][0-9]*$ ]] || { echo 'WORKER_REPLICAS must be positive.' >&2; exit 1; }
# Do not interpolate an image into shell code. Docker accepts it as an argument.
if [[ $# -gt 1 ]]; then echo 'usage: deploy.sh [image:tag or image@sha256:digest]' >&2; exit 2; fi
if [[ $# -eq 1 ]]; then
 [[ "$1" =~ ^[a-zA-Z0-9][a-zA-Z0-9._/:@-]+$ ]] || { echo 'Invalid image reference.' >&2; exit 2; }
 export WORKFLOW_IMAGE="$1"
fi
docker compose config --quiet
docker compose pull postgres migrate api_blue api_green worker caddy
docker compose up -d --wait postgres
# Additive migrations must remain compatible with the previous image.
docker compose run --rm --no-deps migrate
# Keep one healthy API replica while replacing the other.
docker compose up -d --no-deps --wait api_blue
docker compose up -d --no-deps --wait api_green
docker compose up -d --no-deps caddy
docker compose exec -T caddy caddy reload --config /etc/caddy/Caddyfile
# Start replacement workers before draining the previous pool.
docker compose --profile deploy up -d --no-deps --wait --scale "worker_next=$replicas" worker_next
docker compose up -d --no-deps --wait --scale "worker=$replicas" worker
docker compose --profile deploy stop worker_next
docker compose --profile deploy rm -f worker_next
# Persist the successful image without disturbing private environment values.
if [[ $# -eq 1 ]]; then
 task_file=$(mktemp .env.deploy.XXXXXX)
 trap 'rm -f "$task_file"' EXIT
 chmod 600 "$task_file"
 awk '!/^WORKFLOW_IMAGE=/' .env > "$task_file"
 printf 'WORKFLOW_IMAGE=%s\n' "$WORKFLOW_IMAGE" >> "$task_file"
 mv "$task_file" .env
fi
echo 'Deployment is ready. Check runs and Grafana before ending the release.'
