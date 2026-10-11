#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
[[ ! -e .env ]] || { echo '.env exists; edit it instead of replacing credentials.' >&2; exit 1; }
command -v docker >/dev/null
docker compose version >/dev/null
command -v openssl >/dev/null
read -r -p 'Domain name (DNS must point to this host): ' task_domain
read -r -p 'Administrator email / ACME contact: ' task_email
read -r -p 'Application image (GHCR tag or digest): ' task_image
read -r -s -p 'Initial administrator password (at least 12 characters): ' task_password
printf '\n'
[[ "$task_domain" =~ ^[a-zA-Z0-9.-]+$ && "$task_email" =~ ^[a-zA-Z0-9._+@-]+$ && "$task_image" =~ ^[a-zA-Z0-9][a-zA-Z0-9._/:@-]+$ ]] || { echo 'Invalid domain, email, or image.' >&2; exit 2; }
[[ ${#task_password} -ge 12 && "$task_password" != *"'"* && "$task_password" != *$'\n'* && "$task_password" != *$'\r'* ]] || { echo "Use at least 12 characters, without single quotes or newlines." >&2; exit 2; }
task_db_password=$(openssl rand -hex 24)
task_vault_key=$(openssl rand -base64 32)
umask 077
# Password and key values are never printed. Single quotes protect Compose
# interpolation characters in the account password.
{
 printf 'DOMAIN=%s\nACME_EMAIL=%s\nWORKFLOW_IMAGE=%s\n' "$task_domain" "$task_email" "$task_image"
 printf 'POSTGRES_USER=workflow\nPOSTGRES_DB=workflow\nPOSTGRES_PASSWORD=%s\n' "$task_db_password"
 printf 'VAULT_MASTER_KEY=%s\nVAULT_KEY_VERSION=1\n' "$task_vault_key"
 printf "BOOTSTRAP_ADMIN_EMAIL=%s\nBOOTSTRAP_ADMIN_PASSWORD='%s'\n" "$task_email" "$task_password"
 printf 'WORKER_CONCURRENCY=16\nWORKER_BATCH_SIZE=4\nSHUTDOWN_GRACE=30s\n'
} > .env
unset task_password task_db_password task_vault_key
./deploy.sh
echo 'Back up deploy/.env securely: its vault key is required to decrypt backups.'
