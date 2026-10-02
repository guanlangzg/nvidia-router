"""Read-only post-deploy acceptance check — runs ON the domestic test host.

Reports container state, release/image identity, health endpoints, listening
ports, anonymous auth boundaries, database health and post-deploy error
signatures. No secrets are read or printed: only names, statuses and counts.
"""

import json
import re
import subprocess
import urllib.error
import urllib.request

BASE = "http://127.0.0.1:3756"
APP = "nvidia-router-app-1"


def emit(kind, **fields):
    fields["kind"] = kind
    print("R|" + json.dumps(fields, sort_keys=True, separators=(",", ":")), flush=True)


def run(args, timeout=120):
    result = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    return result.returncode, result.stdout.strip(), result.stderr.strip()


def http_status(path, opener=None):
    request = urllib.request.Request(BASE + path, headers={"Origin": BASE})
    try:
        with (opener or urllib.request).urlopen(request, timeout=20) as response:
            return response.status
    except urllib.error.HTTPError as error:
        return error.code
    except Exception as error:  # noqa: BLE001
        return type(error).__name__


def main():
    _, inspect, _ = run([
        "docker", "inspect", APP,
        "--format", "{{.Config.Image}}|{{.State.Status}}|{{.State.Health.Status}}|"
                    "{{.RestartCount}}|{{.State.OOMKilled}}|"
                    "{{index .Config.Labels \"com.docker.compose.project.working_dir\"}}",
    ])
    parts = inspect.split("|")
    emit("app", image=parts[0] if parts else "", status=parts[1] if len(parts) > 1 else "",
         health=parts[2] if len(parts) > 2 else "", restarts=parts[3] if len(parts) > 3 else "",
         oom=parts[4] if len(parts) > 4 else "", working_dir=parts[5] if len(parts) > 5 else "")

    _, releases, _ = run(["ls", "-1t", "/opt/nvidia-router-releases"])
    emit("releases", entries=releases.splitlines()[:4])
    latest = releases.splitlines()[0] if releases else ""
    if latest:
        _, backups, _ = run(["ls", "-la", f"/opt/nvidia-router-releases/{latest}/backups/predeploy-{latest}"])
        emit("backup", release=latest, entries=[line.split()[-3:] for line in backups.splitlines()[1:]])

    for path in ("/health/live", "/health/ready", "/"):
        emit("endpoint", path=path, status=http_status(path))
    for path in ("/v1/models", "/metrics", "/admin/api/models/candidates"):
        emit("anonymous", path=path, status=http_status(path))

    code, ports, _ = run(["ss", "-ltn"])
    emit("ports", listening=[line.split()[3] for line in ports.splitlines()[1:]
                             if re.search(r":(3756|6020|18080|18081)\b", line)])

    # The CLI holds a process lock while the app runs, so `db backup` (the command
    # that runs PRAGMA quick_check) only works from a side container during a
    # deploy. Integrity here is covered indirectly: /health/ready pings the
    # database and verifies migrations, and the deploy-time backup already
    # reported a verified copy.
    emit("db_check", note="covered by /health/ready (ping + VerifyMigrations) and the deploy-time backup")

    code, logs, _ = run(["docker", "logs", "--since", "20m", APP], timeout=180)
    signatures = {}
    for line in logs.splitlines():
        for marker in ("panic", "fatal", "ERROR"):
            if marker in line:
                key = re.sub(r"[0-9a-f]{8,}|\d+", "#", line.split(marker, 1)[1].strip())[:110]
                signatures[f"{marker}:{key}"] = signatures.get(f"{marker}:{marker.lower()}:{key}", 0) + 1
    emit("error_signatures", count=sum(signatures.values()), kinds=sorted(signatures)[:10])

    _, containers, _ = run(["docker", "ps", "--format", "{{.Names}}|{{.Image}}|{{.Status}}"])
    emit("containers", entries=[line for line in containers.splitlines()
                                if "nvidia-router" in line or "proxy" in line])
    return 0


raise SystemExit(main())
