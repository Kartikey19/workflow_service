#!/usr/bin/env python3
"""Inspect resolved Compose JSON privately; never print secret values."""
import base64
import json
import re
import sys
from urllib.parse import parse_qs, urlparse


def validate(config, environment):
    if environment not in {"staging", "production"}:
        raise ValueError("environment must be staging or production")
    services = config["services"]
    for name in ("api", "worker", "migrate"):
        service = services[name]
        env = service["environment"]
        if env.get("APP_ENV") != "production" or env.get("COOKIE_SECURE") != "true":
            raise ValueError("remote environments require production runtime and secure cookies")
        if env.get("EGRESS_ALLOW_PRIVATE", "false").lower() != "false":
            raise ValueError("private egress must remain disabled in remote environments")
        image = service["image"]
        if not re.fullmatch(r"[a-z0-9][a-z0-9._/:-]*@sha256:[a-f0-9]{64}", image):
            raise ValueError("use an immutable image digest")
        uri = urlparse(env.get("DATABASE_URL", ""))
        if uri.scheme not in {"postgres", "postgresql"} or not uri.hostname or not uri.hostname.endswith(".neon.tech"):
            raise ValueError("use a Neon PostgreSQL connection URL")
        if "-pooler" in uri.hostname:
            raise ValueError("use the direct Neon endpoint, with pooling disabled")
        if parse_qs(uri.query).get("sslmode") != ["verify-full"]:
            raise ValueError("Neon URL must use sslmode=verify-full")
        if not env.get("PUBLIC_URL", "").startswith("https://"):
            raise ValueError("set an HTTPS public URL")
        try:
            key = base64.b64decode(env.get("VAULT_MASTER_KEY", ""), validate=True)
        except ValueError:
            key = b""
        if len(key) != 32:
            raise ValueError("set a persistent base64-encoded 32-byte vault master key")
        if service.get("ports"):
            raise ValueError("only the shared Caddy edge may publish ports")
    aliases = services["api"]["networks"]["edge"]["aliases"]
    if f"workflow-{environment}-api" not in aliases:
        raise ValueError("API edge alias must match the selected environment")


if __name__ == "__main__":
    try:
        validate(json.load(sys.stdin), sys.argv[1])
    except (ValueError, KeyError, IndexError, TypeError):
        print("Invalid deployment configuration: check environment, Neon direct endpoint/TLS, vault key and image digest.", file=sys.stderr)
        sys.exit(1)
