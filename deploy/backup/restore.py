#!/usr/bin/env python3
"""Restore into an explicitly selected, empty database. Never overwrite data."""
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from backup import client, settings, sha256, NAME

def restore(source):
    if not os.getenv("RESTORE_DATABASE"):
        raise ValueError("set RESTORE_DATABASE to a separately created empty database")
    target = os.environ["RESTORE_DATABASE"]
    if target == os.getenv("PGDATABASE"):
        raise ValueError("the restore database must differ from the production database")
    env = dict(os.environ, PGDATABASE=target)
    # Inspect all non-system schemas: a target containing existing objects is
    # refused even if its public schema happens to be empty.
    check = subprocess.run(["psql", "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c", "WITH user_schemas AS (SELECT oid,nspname FROM pg_namespace WHERE nspname <> 'information_schema' AND left(nspname,3) <> 'pg_') SELECT (SELECT count(*) FROM pg_class WHERE relnamespace IN (SELECT oid FROM user_schemas)) + (SELECT count(*) FROM pg_proc WHERE pronamespace IN (SELECT oid FROM user_schemas)) + (SELECT count(*) FROM pg_type WHERE typnamespace IN (SELECT oid FROM user_schemas)) + (SELECT count(*) FROM user_schemas WHERE nspname <> 'public')"], env=env, check=True, capture_output=True, text=True)
    if int(check.stdout.strip()) != 0:
        raise ValueError("restore target is not empty")
    with tempfile.TemporaryDirectory(prefix="workflow-restore-") as directory:
        if source.startswith("s3:"):
            _, bucket, prefix = settings()
            name = source.removeprefix("s3:")
            if not bucket or not NAME.fullmatch(name):
                raise ValueError("expected s3:workflow-YYYYMMDDTHHMMSSZ.dump")
            path = Path(directory) / name
            s3 = client()
            metadata = s3.head_object(Bucket=bucket, Key=prefix + name)
            s3.download_file(bucket, prefix + name, str(path))
            checksum = metadata.get("Metadata", {}).get("sha256", "")
        else:
            path = Path(source)
            checksum = path.with_suffix(path.suffix + ".sha256").read_text().strip()
        if sha256(path) != checksum:
            raise ValueError("backup checksum mismatch")
        subprocess.run(["pg_restore", "--no-owner", "--no-acl", "--exit-on-error", "--single-transaction", "--dbname", target, str(path)], env=env, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    print("Restore complete. Verify the restored application before switching traffic.")

if __name__ == "__main__":
    try:
        if len(sys.argv) != 2:
            raise ValueError("usage: restore.py <local dump path | s3:dump-name>")
        restore(sys.argv[1])
    except Exception:
        print("Restore refused or failed. Check the empty target database, source checksum, and connectivity.", file=sys.stderr)
        sys.exit(1)
