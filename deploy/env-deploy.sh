#!/usr/bin/env bash
# Host-side, environment-scoped deployment. Never source the private dotenv file.
set -euo pipefail
cd "$(dirname "$0")"
if [[ $# -ne 2 ]]; then echo 'usage: env-deploy.sh staging|production image@sha256:digest|stop' >&2; exit 2; fi
task_env=$1
[[ "$task_env" == staging || "$task_env" == production ]] || { echo 'Invalid environment.' >&2; exit 2; }
task_release=$2
task_file="$PWD/environments/$task_env/.env"
[[ -f "$task_file" ]] || { echo 'Prepare the selected environment .env first.' >&2; exit 1; }
task_compose=(env -i "PATH=$PATH" "HOME=$HOME" "DEPLOY_ENV=$task_env" "WORKFLOW_ENV_FILE=$task_file")
# Inherited shell application settings must not override another environment's
# private file. Preserve only Docker client configuration, not runtime secrets.
if [[ -n ${DOCKER_CONFIG:-} ]]; then task_compose+=("DOCKER_CONFIG=$DOCKER_CONFIG"); fi
if [[ -n ${DOCKER_HOST:-} ]]; then task_compose+=("DOCKER_HOST=$DOCKER_HOST"); fi
if [[ -n ${DOCKER_CONTEXT:-} ]]; then task_compose+=("DOCKER_CONTEXT=$DOCKER_CONTEXT"); fi
if [[ "$task_release" != stop ]]; then
 [[ "$task_release" =~ ^[a-z0-9][a-z0-9._/:-]*@sha256:[a-f0-9]{64}$ ]] || { echo 'Use an immutable image digest.' >&2; exit 2; }
 task_compose+=("WORKFLOW_IMAGE=$task_release")
fi
task_compose+=(docker compose --project-name "workflow-$task_env" --env-file "$task_file" -f compose.neon.yaml)
command -v flock >/dev/null || { echo 'Install util-linux (flock) on the deployment host.' >&2; exit 1; }
# Serialize environments sharing a host; locks release automatically on failure.
exec 9>.deploy.lock
flock -w 300 9
"${task_compose[@]}" --profile operations config --quiet
if [[ "$task_release" == stop ]]; then
 [[ "$task_env" == staging ]] || { echo 'Only on-demand staging can be stopped by this command.' >&2; exit 2; }
 "${task_compose[@]}" stop api worker
 echo 'Staging stopped; production and stored data remain available.'
 exit
fi
"${task_compose[@]}" --profile operations config --format json | python3 validate-runtime.py "$task_env"
docker network inspect workflow-edge >/dev/null 2>&1 || { echo 'Start deploy/edge first.' >&2; exit 1; }
"${task_compose[@]}" --profile operations pull migrate api worker
# Forward-only migrations must be compatible with the currently running image.
"${task_compose[@]}" --profile operations run --rm --no-deps migrate
"${task_compose[@]}" up -d --no-deps --wait --wait-timeout 180 api worker
# Record only a healthy release. Keep the previous digest for code rollback.
python3 - "$task_file" "$task_release" <<'PY'
import datetime
import os
from pathlib import Path
import sys
import tempfile
path, image = Path(sys.argv[1]), sys.argv[2]
lines = path.read_text().splitlines()
previous = next((line.split("=", 1)[1] for line in lines if line.startswith("WORKFLOW_IMAGE=")), "")
with tempfile.NamedTemporaryFile(mode="w", prefix=".env.deploy.", dir=path.parent, delete=False) as stream:
    os.chmod(stream.name, 0o600)
    stream.write("\n".join(line for line in lines if not line.startswith("WORKFLOW_IMAGE=")) + "\n")
    stream.write("WORKFLOW_IMAGE=" + image + "\n")
    temporary = stream.name
os.replace(temporary, path)
with (path.parent / ".release-history").open("a") as stream:
    stream.write(f"{datetime.datetime.now(datetime.timezone.utc).isoformat()} {image} previous={previous}\n")
PY
echo "$task_env is ready. Verify sign-in, workflow execution and events before promoting."
