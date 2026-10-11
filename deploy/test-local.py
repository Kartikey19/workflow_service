#!/usr/bin/env python3
"""Disposable local deployment acceptance. Requires Docker and a built image.
No production files, volumes, credentials, or external accounts are used.
"""
import base64
import contextlib
import hashlib
import hmac
import http.cookiejar
import http.server
import json
import os
import platform
from pathlib import Path
import secrets
import shutil
import socket
import ssl
import subprocess
import tempfile
import threading
import time
import urllib.request

ROOT = Path(__file__).resolve().parent

def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]

def main():
    image = os.getenv("LOCAL_TEST_IMAGE", "workflow-service:acceptance")
    https_port, restored_port, upstream_port = free_port(), free_port(), free_port()
    called = threading.Event()
    class Upstream(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            called.set()
            time.sleep(12)
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"result":"completed during deployment"}')
        def log_message(self, *_):
            pass
    upstream = http.server.ThreadingHTTPServer(("0.0.0.0", upstream_port), Upstream)
    upstream.daemon_threads = True
    thread = threading.Thread(target=upstream.serve_forever, daemon=True)
    thread.start()
    project = "workflow-acceptance-" + secrets.token_hex(3)
    with tempfile.TemporaryDirectory(prefix="workflow-local-") as directory:
        work = Path(directory)
        shutil.copy(ROOT / "compose.yaml", work / "compose.yaml")
        shutil.copy(ROOT / "Caddyfile", work / "Caddyfile")
        shutil.copytree(ROOT / "backup", work / "backup", ignore=shutil.ignore_patterns("__pycache__"))
        shutil.copytree(ROOT / "observability", work / "observability")
        password = secrets.token_hex(24)
        admin_password = secrets.token_hex(16)
        vault_key = base64.b64encode(secrets.token_bytes(32)).decode()
        env_file = work / ".env"
        env_file.write_text(f"WORKFLOW_IMAGE={image}\nDOMAIN=localhost\nACME_EMAIL=local@example.com\nPOSTGRES_USER=workflow\nPOSTGRES_DB=workflow\nPOSTGRES_PASSWORD={password}\nBOOTSTRAP_ADMIN_EMAIL=local@example.com\nBOOTSTRAP_ADMIN_PASSWORD={admin_password}\nVAULT_MASTER_KEY={vault_key}\nVAULT_KEY_VERSION=1\nBACKUP_LOCAL_ONLY=true\n")
        env_file.chmod(0o600)
        app_env = {"PUBLIC_URL": f"https://localhost:{https_port}", "EGRESS_ALLOW_PRIVATE": "true", "SHUTDOWN_GRACE": "20s"}
        override = {"services": {name: {"environment": app_env} for name in ["api_blue", "api_green", "worker", "worker_next"]}}
        if platform.system() == "Linux":
            for service in ["api_blue", "api_green", "worker", "worker_next"]:
                override["services"][service]["extra_hosts"] = ["host.docker.internal:host-gateway"]
        override["services"]["caddy"] = {"ports": []}
        # Compose's !override prevents accidentally exposing the production ports.
        text = json.dumps(override)
        (work / "local.yaml").write_text(text.replace('"ports": []', f'"ports": !override ["127.0.0.1:{https_port}:443"]'))
        # A separate API proves the restored data can be used with the same key.
        restored = {"services": {"restored_api": {"image": image, "env_file": ".env", "profiles": ["test"], "environment": {"DATABASE_URL": f"postgres://workflow:{password}@postgres:5432/workflow_restored?sslmode=disable", "HTTP_ADDRESS": ":8080", "APP_ENV": "testing", "EGRESS_ALLOW_PRIVATE": "true"}, "ports": [f"127.0.0.1:{restored_port}:8080"], "networks": ["backend"]}}}
        (work / "restore.yaml").write_text(json.dumps(restored))
        compose = ["docker", "compose", "-p", project, "-f", "compose.yaml", "-f", "local.yaml", "-f", "restore.yaml"]
        def run(*args, check=True, capture=True):
            result = subprocess.run(compose + list(args), cwd=work, stdout=subprocess.PIPE if capture else None, stderr=subprocess.PIPE if capture else None)
            if check and result.returncode:
                diagnostics = subprocess.run(compose + ["logs", "--tail", "25"], cwd=work, capture_output=True).stdout.decode(errors="replace")
                for secret in [password, admin_password, vault_key]:
                    diagnostics = diagnostics.replace(secret, "***")
                print(diagnostics, flush=True)
                raise RuntimeError("local container command failed: " + " ".join(args[:4]))
            return result
        jar = http.cookiejar.CookieJar()
        opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar), urllib.request.HTTPSHandler(context=ssl._create_unverified_context()))
        base = f"https://localhost:{https_port}"
        def request(path, body=None, method=None, headers=None, origin=None):
            raw = json.dumps(body).encode() if body is not None else None
            req = urllib.request.Request((origin or base) + path, raw, {"Content-Type": "application/json", **(headers or {})}, method=method)
            with opener.open(req, timeout=10) as response:
                data = response.read()
                return json.loads(data) if data and "application/json" in response.headers.get("Content-Type", "") else data
        try:
            print("Starting isolated PostgreSQL, API replicas, workers, and local HTTPS proxy…", flush=True)
            run("config", "--quiet")
            run("up", "-d", "--wait", "postgres")
            run("run", "--rm", "--no-deps", "migrate")
            run("run", "--rm", "--no-deps", "migrate")
            run("up", "-d", "--no-deps", "--wait", "api_blue", "api_green", "worker")
            run("up", "-d", "--no-deps", "caddy")
            deadline = time.monotonic() + 45
            while True:
                try:
                    request("/readyz")
                    break
                except Exception:
                    if time.monotonic() > deadline:
                        raise RuntimeError("local HTTPS readiness failed")
                    time.sleep(1)
            assert b'<div id="root">' in request("/")
            request("/v1/auth/login", {"email": "local@example.com", "password": admin_password})
            source = f"apiVersion: workflow/v1\nname: deployment-test\nnodes:\n  - id: fetch\n    type: http\n    timeout: 40s\n    config:\n      method: GET\n      url: http://host.docker.internal:{upstream_port}/slow\noutputs:\n  response: '${{nodes.fetch.output.body}}'\n"
            request("/v1/workflows", {"yaml": source})
            request("/v1/secrets/RESTORE_TEST", {"value": "a sealed test value"}, "PUT")
            hook = request("/v1/triggers", {"name": "restore-test", "workflow": "deployment-test", "signed": True})
            key = request("/v1/api-keys", {"name": "restore-check", "role": "admin"})["token"]
            run_id = request("/v1/runs", {"workflow": "deployment-test", "input": {}})["id"]
            assert called.wait(15), "worker never started the HTTP node"
            readiness_errors = []
            stop_probe = threading.Event()
            def probe():
                while not stop_probe.wait(.15):
                    try:
                        request("/readyz")
                    except Exception:
                        readiness_errors.append(True)
            probing = threading.Thread(target=probe, daemon=True)
            probing.start()
            print("Replacing API replicas and workers during a running HTTP node…", flush=True)
            try:
                run("up", "-d", "--no-deps", "--wait", "--force-recreate", "api_blue")
                run("up", "-d", "--no-deps", "--wait", "--force-recreate", "api_green")
                # Ensure a node is actively running when the worker is replaced,
                # rather than relying on it still being active after API checks.
                called.clear()
                run_id = request("/v1/runs", {"workflow": "deployment-test", "input": {}})["id"]
                assert called.wait(15)
                run("--profile", "deploy", "up", "-d", "--no-deps", "--wait", "worker_next")
                run("up", "-d", "--no-deps", "--wait", "--force-recreate", "worker")
                run("--profile", "deploy", "stop", "worker_next")
            finally:
                stop_probe.set()
                probing.join(15)
            assert not readiness_errors, "HTTPS readiness interrupted during API replacement"
            deadline = time.monotonic() + 45
            while True:
                status = request(f"/v1/runs/{run_id}")["status"]
                if status == "succeeded":
                    break
                assert status not in ["failed", "cancelled", "timed_out"], "mid-deploy run failed"
                assert time.monotonic() < deadline
                time.sleep(.5)
            outcomes = request("/v1/runs")["items"]
            assert all(r["status"] == "succeeded" for r in outcomes), "some deployment runs did not succeed"
            print("Creating a backup and restoring it into a separate empty database…", flush=True)
            run("--profile", "backups", "build", "backup")
            run("--profile", "backups", "run", "--rm", "--no-deps", "backup", "python3", "/usr/local/bin/test_backup.py")
            run("--profile", "backups", "run", "--rm", "--no-deps", "backup", "python3", "/usr/local/bin/backup.py")
            run("exec", "-T", "postgres", "createdb", "-U", "workflow", "workflow_restored")
            name = run("--profile", "backups", "run", "--rm", "--no-deps", "backup", "sh", "-c", "ls -1 /backups/workflow-*.dump | tail -1").stdout.decode().strip()
            run("--profile", "backups", "run", "--rm", "--no-deps", "-e", "RESTORE_DATABASE=workflow_restored", "backup", "python3", "/usr/local/bin/restore.py", name)
            # Repeating against a populated database must fail before overwriting.
            refused = run("--profile", "backups", "run", "--rm", "--no-deps", "-e", "RESTORE_DATABASE=workflow_restored", "backup", "python3", "/usr/local/bin/restore.py", name, check=False)
            assert refused.returncode != 0, "restore overwrote a populated database"
            run("exec", "-T", "postgres", "createdb", "-U", "workflow", "workflow_function_target")
            run("exec", "-T", "postgres", "psql", "-U", "workflow", "-d", "workflow_function_target", "-c", "CREATE FUNCTION keep_me() RETURNS integer LANGUAGE SQL AS 'select 1'")
            refused = run("--profile", "backups", "run", "--rm", "--no-deps", "-e", "RESTORE_DATABASE=workflow_function_target", "backup", "python3", "/usr/local/bin/restore.py", name, check=False)
            assert refused.returncode != 0, "restore accepted a database containing an existing function"
            original_count = run("exec", "-T", "postgres", "psql", "-U", "workflow", "-d", "workflow", "-Atc", "select count(*) from runs").stdout
            restored_count = run("exec", "-T", "postgres", "psql", "-U", "workflow", "-d", "workflow_restored", "-Atc", "select count(*) from runs").stdout
            assert original_count == restored_count
            run("--profile", "test", "up", "-d", "--no-deps", "restored_api")
            restored_base = f"http://localhost:{restored_port}"
            deadline = time.monotonic() + 30
            while True:
                try:
                    request("/readyz", origin=restored_base)
                    break
                except Exception:
                    if time.monotonic() > deadline:
                        raise RuntimeError("restored API readiness failed")
                    time.sleep(.5)
            assert request("/v1/workflows", headers={"Authorization": "Bearer " + key}, origin=restored_base)["items"][0]["name"] == "deployment-test"
            # HMAC verification decrypts the signing secret from the restored DB.
            body = json.dumps({}).encode()
            signature = hmac.new(hook["signing_secret"].encode(), body, hashlib.sha256).hexdigest()
            replay = request("/hooks/" + hook["token"], {}, headers={"X-Workflow-Signature": "sha256=" + signature}, origin=restored_base)
            assert replay["trigger"] == "webhook" and replay["id"]
            print("PASS: local HTTPS, migrations, API availability, mid-run worker replacement, backup checksum, restore refusal, and restored webhook decryption.", flush=True)
        finally:
            run("--profile", "test", "--profile", "deploy", "--profile", "backups", "down", "--volumes", "--remove-orphans", check=False)
            upstream.shutdown()
            upstream.server_close()
            thread.join(5)

if __name__ == "__main__":
    main()
