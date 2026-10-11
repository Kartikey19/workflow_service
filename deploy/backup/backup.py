#!/usr/bin/env python3
"""Custom-format snapshots, SHA-256 verification, narrowly scoped retention."""
import datetime as dt
import hashlib
import os
from pathlib import Path
import re
import subprocess
import sys
import boto3

NAME = re.compile(r"workflow-\d{8}T\d{6}Z\.dump$")

def settings():
    days = int(os.getenv("BACKUP_RETENTION_DAYS", "14"))
    if not 1 <= days <= 3650:
        raise ValueError("BACKUP_RETENTION_DAYS must be between 1 and 3650")
    bucket = os.getenv("BACKUP_BUCKET", "")
    prefix = os.getenv("BACKUP_PREFIX", "workflow/backups/")
    if not prefix.endswith("/") or prefix.startswith("/") or len(prefix.strip("/")) < 3 or ".." in prefix:
        raise ValueError("BACKUP_PREFIX must be a nonempty dedicated prefix ending with /")
    return days, bucket, prefix

def client():
    return boto3.client("s3", endpoint_url=os.getenv("AWS_ENDPOINT_URL") or None)

def sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()

def backup():
    os.umask(0o077)
    days, bucket, prefix = settings()
    local_only = os.getenv("BACKUP_LOCAL_ONLY") == "true"
    if not bucket and not local_only:
        raise ValueError("BACKUP_BUCKET is required (or explicitly set BACKUP_LOCAL_ONLY=true)")
    root = Path(os.getenv("BACKUP_DIRECTORY", "/backups"))
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    now = dt.datetime.now(dt.timezone.utc)
    name = now.strftime("workflow-%Y%m%dT%H%M%SZ.dump")
    path = root / name
    temporary = root / (name + ".partial")
    try:
        subprocess.run(["pg_dump", "--format=custom", "--no-owner", "--no-acl", "--file", str(temporary)], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        checksum = sha256(temporary)
        temporary.replace(path)
        (root / (name + ".sha256")).write_text(checksum + "\n")
        if bucket:
            s3 = client()
            extra = {"Metadata": {"sha256": checksum}, "ServerSideEncryption": os.getenv("BACKUP_SSE", "AES256")}
            if os.getenv("BACKUP_KMS_KEY_ID"):
                extra["SSEKMSKeyId"] = os.environ["BACKUP_KMS_KEY_ID"]
            s3.upload_file(str(path), bucket, prefix + name, ExtraArgs=extra)
            stored = s3.head_object(Bucket=bucket, Key=prefix + name)
            if stored["ContentLength"] != path.stat().st_size or stored.get("Metadata", {}).get("sha256") != checksum:
                raise RuntimeError("uploaded backup verification failed")
            cutoff = now - dt.timedelta(days=days)
            for page in s3.get_paginator("list_objects_v2").paginate(Bucket=bucket, Prefix=prefix):
                for item in page.get("Contents", []):
                    suffix = item["Key"][len(prefix):]
                    if NAME.fullmatch(suffix) and item["LastModified"] < cutoff:
                        s3.delete_object(Bucket=bucket, Key=item["Key"])
        # Remove local copies only after a successful snapshot/upload.
        cutoff = now.timestamp() - days * 86400
        for old in root.glob("workflow-*.dump"):
            if NAME.fullmatch(old.name) and old.stat().st_mtime < cutoff:
                old.unlink()
                old.with_suffix(".dump.sha256").unlink(missing_ok=True)
        print(f"Backup complete: {name}", flush=True)
    finally:
        temporary.unlink(missing_ok=True)

if __name__ == "__main__":
    try:
        backup()
    except Exception:
        # SDK/pg_dump exception strings can contain endpoints or credential data.
        print("Backup failed; check database/S3 connectivity, permissions, and configuration.", file=sys.stderr)
        sys.exit(1)
