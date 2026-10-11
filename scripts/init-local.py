#!/usr/bin/env python3
"""Create local development settings once; preserve existing credentials."""
import base64
import os
from pathlib import Path
import secrets

root = Path(__file__).resolve().parent.parent
path = root / ".env"
if path.exists():
    print("Existing .env preserved. See README for the local database settings.")
else:
    password = secrets.token_hex(24)
    admin_password = secrets.token_urlsafe(24)
    vault_key = base64.b64encode(secrets.token_bytes(32)).decode()
    settings = f"""# Local development only. Keep this file private and out of Git.
LOCAL_POSTGRES_PORT=55433
LOCAL_POSTGRES_PASSWORD={password}
DATABASE_URL=postgres://workflow:{password}@127.0.0.1:55433/workflow_local?sslmode=disable
APP_ENV=development
HTTP_ADDRESS=127.0.0.1:8080
PUBLIC_URL=http://localhost:8080
COOKIE_SECURE=false
BOOTSTRAP_ADMIN_EMAIL=admin@localhost.test
BOOTSTRAP_ADMIN_PASSWORD={admin_password}
VAULT_MASTER_KEY={vault_key}
VAULT_KEY_VERSION=1
VAULT_OLD_KEYS=
WORKER_CONCURRENCY=4
WORKER_BATCH_SIZE=4
WORKER_METRICS_ADDRESS=127.0.0.1:9091
DB_MAX_CONNS=5
# Permit your local database/HTTP connectors during local testing only.
EGRESS_ALLOW_PRIVATE=true
ANTHROPIC_API_KEY=
ASSISTANT_PROVIDER=anthropic
ASSISTANT_API_KEY=
ASSISTANT_MODEL=
LLM_PRICING_JSON=
GOOGLE_CLIENT_ID=
GOOGLE_CLIENT_SECRET=
DROPBOX_APP_KEY=
DROPBOX_APP_SECRET=
MICROSOFT_CLIENT_ID=
MICROSOFT_CLIENT_SECRET=
MICROSOFT_TENANT=common
"""
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as output:
        output.write(settings)
    print("Created private .env. Bootstrap login credentials are stored in that file.")
