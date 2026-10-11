#!/usr/bin/env python3
"""Deploy a DOCR digest without replacing App Platform's runtime configuration."""

import argparse
import copy
import json
import re
import subprocess
import sys
import uuid


def release_parts(image):
    match = re.fullmatch(
        r"registry\.digitalocean\.com/([a-z0-9][a-z0-9-]*)/"
        r"([a-z0-9]+(?:[._-][a-z0-9]+)*)@(sha256:[a-f0-9]{64})", image
    )
    if not match:
        raise ValueError("release must be a DOCR image@sha256:digest (no mutable tags)")
    return match.groups()


def release_spec(current, image):
    registry, repository, digest = release_parts(image)
    spec = copy.deepcopy(current)
    services = [s for s in spec.get("services", []) if s.get("name") == "web"]
    if len(services) != 1:
        raise ValueError("app must have exactly one service named web; apply .do/app.yaml first")
    service = services[0]
    previous = service.get("image", {})
    if previous.get("registry_type") != "DOCR":
        raise ValueError("web must already use DOCR; apply .do/app.yaml first")
    if previous.get("registry") and previous["registry"] != registry:
        raise ValueError("release registry does not match the app's registry")
    if previous.get("repository") != repository:
        raise ValueError("release repository does not match the app's repository")
    if any(service.get(key) for key in ("git", "github", "gitlab", "bitbucket")):
        raise ValueError("web still has a Git source; apply .do/app.yaml first")
    # Preserve encrypted env values, domains, sizing and all other app settings.
    previous.pop("tag", None)
    previous["digest"] = digest
    previous["deploy_on_push"] = {"enabled": False}
    service["image"] = previous
    return spec


def deploy(app_id, image):
    try:
        uuid.UUID(app_id)
    except ValueError:
        raise ValueError("DO_APP_ID must be an App Platform UUID") from None
    release_parts(image)  # Reject bad input before contacting DigitalOcean.
    result = subprocess.run(
        ["doctl", "apps", "spec", "get", app_id, "--format", "json"],
        check=True, capture_output=True, text=True, timeout=60,
    )
    spec = release_spec(json.loads(result.stdout), image)
    # Send the spec directly over stdin; do not log it or persist runtime secrets.
    subprocess.run(
        ["doctl", "apps", "update", app_id, "--spec", "-", "--update-sources",
         "--wait", "--format", "ID,DefaultIngress"],
        input=json.dumps(spec), text=True, check=True, timeout=18 * 60,
    )
    print(f"Deployed {image}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("app_id")
    parser.add_argument("image")
    args = parser.parse_args()
    try:
        deploy(args.app_id, args.image)
    except (ValueError, KeyError, TypeError):
        print("Invalid app ID, immutable release image or app spec. Check .do/app.yaml and DO_APP_ID.", file=sys.stderr)
        return 1
    except (subprocess.SubprocessError, OSError):
        # A captured CLI error may contain spec values; never echo it wholesale.
        print("App Platform deployment failed. Check doctl authentication and the app's deployment status.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
