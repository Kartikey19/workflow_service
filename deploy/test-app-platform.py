#!/usr/bin/env python3
"""Release safety tests; API/queue behavior is tested against real PostgreSQL."""

import copy
import importlib.util
import json
import subprocess
import unittest
from pathlib import Path
from unittest.mock import patch

module_spec = importlib.util.spec_from_file_location("app_platform", Path(__file__).with_name("app-platform.py"))
platform = importlib.util.module_from_spec(module_spec)
module_spec.loader.exec_module(platform)
APP_ID = "f81d4fae-7dec-11d0-a765-00a0c91e6bf6"
IMAGE = "registry.digitalocean.com/test-registry/workflow-service@sha256:" + "a" * 64


class ReleaseTest(unittest.TestCase):
    def setUp(self):
        self.current = {
            "name": "weft", "region": "nyc",
            "domains": [{"domain": "app.example.com"}],
            "envs": [{"key": "DATABASE_URL", "type": "SECRET", "value": "EV[fixture]"}],
            "services": [{"name": "web", "instance_count": 1,
                          "image": {"registry_type": "DOCR", "repository": "workflow-service",
                                    "tag": "first-release", "deploy_on_push": {"enabled": True}},
                          "envs": [{"key": "VAULT_MASTER_KEY", "type": "SECRET", "value": "EV[key]"}]}],
        }

    def test_only_image_changes_and_original_is_preserved(self):
        original = copy.deepcopy(self.current)
        result = platform.release_spec(self.current, IMAGE)
        self.assertEqual(self.current, original)
        expected = copy.deepcopy(original)
        expected["services"][0]["image"] = {
            "registry_type": "DOCR", "repository": "workflow-service",
            "digest": "sha256:" + "a" * 64, "deploy_on_push": {"enabled": False},
        }
        self.assertEqual(result, expected)
        # A rollback changes only the digest again, including from a pinned spec.
        rollback = platform.release_spec(result, IMAGE.replace("a" * 64, "b" * 64))
        self.assertEqual(rollback["services"][0]["image"]["digest"], "sha256:" + "b" * 64)

    def test_rejects_mutable_or_foreign_images_before_network_access(self):
        for image in [IMAGE.split("@")[0] + ":latest", IMAGE.replace("registry.digitalocean.com", "ghcr.io"),
                      IMAGE[:-1], IMAGE + "\n"]:
            with self.subTest(image=image), patch.object(platform.subprocess, "run") as run:
                with self.assertRaises(ValueError):
                    platform.deploy(APP_ID, image)
                run.assert_not_called()

    def test_rejects_wrong_app_source_or_repository(self):
        for change in [{"name": "another-service"}, {"github": {"repo": "owner/repo"}},
                       {"image": {"registry_type": "GHCR"}},
                       {"image": {"registry_type": "DOCR", "repository": "other"}},
                       {"image": {"registry_type": "DOCR", "registry": "other", "repository": "workflow-service"}}]:
            with self.subTest(change=change):
                current = copy.deepcopy(self.current)
                current["services"][0].update(change)
                with self.assertRaises(ValueError):
                    platform.release_spec(current, IMAGE)

    def test_deploy_sends_private_spec_over_stdin_and_waits(self):
        with patch.object(platform.subprocess, "run") as run:
            run.side_effect = [subprocess.CompletedProcess([], 0, json.dumps(self.current)),
                               subprocess.CompletedProcess([], 0)]
            platform.deploy(APP_ID, IMAGE)
            update = run.call_args_list[1]
            self.assertIn("--wait", update.args[0])
            self.assertIn("--update-sources", update.args[0])
            self.assertIn("-", update.args[0])
            sent = json.loads(update.kwargs["input"])
            self.assertEqual(sent["envs"], self.current["envs"])
            self.assertTrue(update.kwargs["check"])

    def test_cli_failure_prevents_success(self):
        with patch.object(platform.subprocess, "run") as run:
            run.side_effect = [subprocess.CompletedProcess([], 0, json.dumps(self.current)),
                               subprocess.CalledProcessError(1, "doctl")]
            with self.assertRaises(subprocess.CalledProcessError):
                platform.deploy(APP_ID, IMAGE)


if __name__ == "__main__":
    unittest.main()
