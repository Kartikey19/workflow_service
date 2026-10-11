#!/usr/bin/env python3
"""Remote config validation and host release failure/stop behavior, without cloud writes."""
import base64
import copy
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("validate_runtime", ROOT / "validate-runtime.py")
validator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(validator)
IMAGE = "ghcr.io/example/workflow@sha256:" + "a" * 64
PREVIOUS = "ghcr.io/example/workflow@sha256:" + "b" * 64


def config(environment="staging"):
    env = {"APP_ENV": "production", "COOKIE_SECURE": "true", "PUBLIC_URL": "https://staging.example.com",
           "DATABASE_URL": "postgresql://user:private-value@ep-example.aws.neon.tech/workflow?sslmode=verify-full",
           "VAULT_MASTER_KEY": base64.b64encode(b"v" * 32).decode()}
    app = {"image": IMAGE, "environment": env, "networks": {"backend": {}, "edge": {"aliases": [f"workflow-{environment}-api"]}}}
    return {"services": {name: copy.deepcopy(app) for name in ("api", "worker", "migrate")}}


class RuntimeTests(unittest.TestCase):
    def test_actual_compose_environment_configs(self):
        with tempfile.TemporaryDirectory(prefix="workflow-compose-test-") as directory:
            path = Path(directory) / ".env"
            path.write_text(f"DOMAIN=staging.example.com\nWORKFLOW_IMAGE={IMAGE}\nDATABASE_URL='postgresql://u:private-value@ep-example.aws.neon.tech/workflow?sslmode=verify-full'\nVAULT_MASTER_KEY={base64.b64encode(b'v' * 32).decode()}\n")
            for environment in ("staging", "production"):
                env = {name: os.environ[name] for name in ("PATH", "HOME")}
                env.update(DEPLOY_ENV=environment, WORKFLOW_ENV_FILE=str(path))
                result = subprocess.run(["docker", "compose", "--project-name", f"workflow-{environment}", "--env-file", str(path), "-f", str(ROOT / "compose.neon.yaml"), "--profile", "operations", "config", "--format", "json"], env=env, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, "Compose configuration should resolve")
                actual = json.loads(result.stdout)
                validator.validate(actual, environment)
                self.assertEqual(actual["name"], f"workflow-{environment}")
                self.assertEqual(set(actual["services"]), {"api", "worker", "migrate"})
                self.assertEqual(actual["networks"]["backend"]["name"], f"workflow-{environment}_backend")

    def test_valid_and_unsafe_configs(self):
        for environment in ("staging", "production"):
            validator.validate(config(environment), environment)
        for name, value in (("APP_ENV", "development"), ("COOKIE_SECURE", "false"), ("EGRESS_ALLOW_PRIVATE", "true"),
                            ("VAULT_MASTER_KEY", "bad"), ("PUBLIC_URL", "http://example.com"),
                            ("DATABASE_URL", "postgresql://u:p@ep-example-pooler.aws.neon.tech/db?sslmode=verify-full"),
                            ("DATABASE_URL", "postgresql://u:p@ep-example.aws.neon.tech/db?sslmode=require")):
            bad = config()
            bad["services"]["api"]["environment"][name] = value
            with self.subTest(name=name), self.assertRaises(ValueError):
                validator.validate(bad, "staging")
        for mutate in (lambda c: c["services"]["worker"].update(image="workflow:latest"),
                       lambda c: c["services"]["api"].update(ports=["8080:8080"]),
                       lambda c: c["services"]["api"]["networks"]["edge"].update(aliases=["workflow-production-api"])):
            bad = config()
            mutate(bad)
            with self.assertRaises(ValueError):
                validator.validate(bad, "staging")

    def test_deploy_failure_and_stop_are_environment_scoped(self):
        with tempfile.TemporaryDirectory(prefix="workflow-deploy-test-") as directory:
            root = Path(directory)
            for name in ("env-deploy.sh", "validate-runtime.py", "compose.neon.yaml"):
                shutil.copy(ROOT / name, root / name)
            for environment in ("staging", "production"):
                target = root / "environments" / environment
                target.mkdir(parents=True)
                (target / ".env").write_text(f"WORKFLOW_IMAGE={PREVIOUS}\nKEEP_SECRET=private-value\n")
            mock = root / "bin"
            mock.mkdir()
            (root / "config.json").write_text(json.dumps(config()))
            (mock / "docker").write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
args=sys.argv[1:]
root=pathlib.Path(__file__).resolve().parent.parent
if args and args[0] == "compose":
    assert not os.getenv("DATABASE_URL"), "inherited database URL leaked across environments"
with (root / "calls").open("a") as f: f.write(json.dumps(args)+"\\n")
if "--format" in args: print((root / "config.json").read_text())
if "up" in args and (root / "FAIL_UP").exists(): sys.exit(42)
if "run" in args and "migrate" in args and (root / "FAIL_MIGRATE").exists(): sys.exit(43)
''')
            # macOS does not ship flock; this test examines release semantics,
            # while Linux hosts and CI use util-linux's real lock.
            (mock / "flock").write_text("#!/bin/sh\nexit 0\n")
            for path in mock.iterdir():
                path.chmod(0o755)
            env = dict(os.environ, PATH=str(mock) + os.pathsep + os.environ["PATH"], DATABASE_URL="postgresql://wrong-environment/private-value")
            staging = root / "environments/staging/.env"
            production = root / "environments/production/.env"
            def run(environment, image, extra=None):
                for marker in ("FAIL_UP", "FAIL_MIGRATE"):
                    (root / marker).unlink(missing_ok=True)
                    if (extra or {}).get(marker) == "true":
                        (root / marker).touch()
                result = subprocess.run(["bash", str(root / "env-deploy.sh"), environment, image], env=env, capture_output=True, text=True)
                self.assertNotIn("private-value", result.stdout + result.stderr)
                return result
            self.assertEqual(run("staging", IMAGE, {"FAIL_MIGRATE": "true"}).returncode, 43)
            calls = [json.loads(line) for line in (root / "calls").read_text().splitlines()]
            self.assertFalse(any("up" in call for call in calls))
            self.assertIn(PREVIOUS, staging.read_text())
            self.assertEqual(run("staging", IMAGE, {"FAIL_UP": "true"}).returncode, 42)
            self.assertIn(PREVIOUS, staging.read_text())
            self.assertFalse((staging.parent / ".release-history").exists())
            self.assertEqual(run("staging", IMAGE).returncode, 0)
            self.assertIn(IMAGE, staging.read_text())
            self.assertIn("KEEP_SECRET=private-value", staging.read_text())
            self.assertIn(PREVIOUS, production.read_text())
            history = (staging.parent / ".release-history").read_text()
            self.assertIn("previous=" + PREVIOUS, history)
            before = staging.read_text()
            self.assertEqual(run("staging", "stop").returncode, 0)
            self.assertEqual(staging.read_text(), before)
            self.assertNotEqual(run("production", "stop").returncode, 0)
            calls = [json.loads(line) for line in (root / "calls").read_text().splitlines()]
            self.assertTrue(any("migrate" in call and "run" in call for call in calls))
            self.assertTrue(any(call[-3:] == ["stop", "api", "worker"] for call in calls))
            self.assertFalse(any("down" in call for call in calls))


if __name__ == "__main__":
    unittest.main()
