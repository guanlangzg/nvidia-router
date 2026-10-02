#!/usr/bin/env python3
"""Deploy the router to the domestic host.

Packages the current HEAD, uploads it as a new release, inherits the running
release's .env and deploy override, builds the image, takes a pre-migration
database backup with the OLD image, then swaps to the new one and verifies
health. Every step prints its outcome; the first failure stops the run.

Secrets are never read, printed or copied through this machine: .env is copied
host-side with its permissions preserved.

Usage: python scripts/deploy/deploy_remote.py <tag>
"""

from __future__ import annotations

import subprocess
import sys
import threading
from collections import deque
from pathlib import Path

import paramiko

REPO = Path(__file__).resolve().parents[2]
SSH_CONFIG = REPO.parents[0] / "服务器管理" / "hangzhou2-2" / "ssh_config_local"
RELEASES = "/opt/nvidia-router-releases"


def connect() -> paramiko.SSHClient:
    config = paramiko.SSHConfig()
    with SSH_CONFIG.open(encoding="utf-8") as handle:
        config.parse(handle)
    host = config.lookup("hangzhou2-2")
    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    client.connect(
        host["hostname"], port=int(host.get("port", 22)), username=host.get("user", "root"),
        key_filename=host["identityfile"][0], timeout=30,
    )
    return client


def run(client: paramiko.SSHClient, command: str, timeout: int = 1800, check: bool = True) -> str:
    print(f"$ {command}", flush=True)
    _, stdout, stderr = client.exec_command(command, timeout=timeout)
    channel = stdout.channel
    output_lines: deque[str] = deque(maxlen=200)
    errors: deque[str] = deque(maxlen=200)
    lock = threading.Lock()

    def drain(stream: paramiko.ChannelFile, lines: deque[str], target: object) -> None:
        for line in stream:
            decoded = line.decode("utf-8", "replace") if isinstance(line, bytes) else line
            with lock:
                lines.append(decoded.rstrip("\r\n"))
            print(decoded, end="", file=target, flush=True)

    stdout_thread = threading.Thread(target=drain, args=(stdout, output_lines, sys.stdout))
    stderr_thread = threading.Thread(target=drain, args=(stderr, errors, sys.stderr))
    stdout_thread.start()
    stderr_thread.start()
    stdout_thread.join(timeout)
    stderr_thread.join(timeout)
    if stdout_thread.is_alive() or stderr_thread.is_alive():
        channel.close()
        raise TimeoutError(f"remote command timed out after {timeout}s: {command}")

    status = channel.recv_exit_status()
    if status != 0:
        error = "\n".join(errors)
        if check:
            raise SystemExit(f"step failed (exit {status}): {command}\n{error[-2000:]}")
    return "\n".join(output_lines)


def main() -> int:
    if len(sys.argv) < 2:
        raise SystemExit("usage: deploy_remote.py <tag>")
    tag = sys.argv[1]
    release = f"{RELEASES}/{tag}"
    image = f"nvidia-router:deploy-{tag}"

    archive = REPO / "tmp" / f"release-{tag}.tar"
    archive.parent.mkdir(parents=True, exist_ok=True)
    subprocess.run(["git", "archive", "HEAD", "-o", str(archive)], cwd=REPO, check=True)
    print(f"packaged {archive.name} ({archive.stat().st_size} bytes)", flush=True)

    client = connect()
    try:
        current = run(client, "docker inspect nvidia-router-app-1 --format "
                              "'{{index .Config.Labels \"com.docker.compose.project.working_dir\"}}'").strip()
        previous_image = run(client, "docker inspect nvidia-router-app-1 --format '{{.Config.Image}}'").strip()
        if not current or not previous_image:
            raise SystemExit("cannot determine the running release; aborting")
        print(f"current release: {current}\ncurrent image:   {previous_image}", flush=True)

        # A release directory left behind by a failed build carries nothing but
        # extracted source and a copy of .env; a half-built release must never be
        # reused, so start it clean. Anything with a backup in it is refused.
        run(client, f"test ! -e {release}/backups || (echo 'refusing to reuse a release that holds backups' && false)")
        run(client, f"rm -rf {release}")
        sftp = client.open_sftp()
        sftp.put(str(archive), f"/tmp/release-{tag}.tar")
        sftp.close()
        run(client, f"mkdir -p {release} && tar -xf /tmp/release-{tag}.tar -C {release} && rm -f /tmp/release-{tag}.tar")
        # .env carries the master key and upstream URL: copy host-side, preserving 0600.
        run(client, f"cp -p {current}/.env {release}/.env && cp -p {current}/docker-compose.deploy.yml {release}/")
        run(client, f"ls -la {release}/.env")

        print("\n=== build ===", flush=True)
        # proxy.golang.org is unreachable from the mainland host; the Dockerfile
        # exposes GOPROXY exactly for this.
        run(client, f"cd {release} && docker build --build-arg GOPROXY=https://goproxy.cn,direct "
                    f"-t {image} .", timeout=3600)

        print("\n=== stop app ===", flush=True)
        compose = (f"cd {release} && NVIDIA_ROUTER_IMAGE={image} docker compose "
                   f"-p nvidia-router -f docker-compose.yml -f docker-compose.deploy.yml")
        run(client, f"{compose} stop app")

        print("\n=== pre-migration backup (old image) ===", flush=True)
        backup = f"{release}/backups/predeploy-{tag}"
        run(client, f"mkdir -p {backup} && chown 10001:10001 {backup}")
        run(client, f"docker run --rm --user 10001:10001 -v nvr-data:/data "
                    f"-v {backup}:/backup {previous_image} db backup --output /backup/router.db")
        run(client, f"ls -la {backup}")

        print("\n=== start new image ===", flush=True)
        run(client, f"{compose} up -d app", timeout=900)

        print("\n=== health ===", flush=True)
        run(client, "sleep 8 && curl -fsS http://127.0.0.1:3756/health/live", check=False)
        run(client, "curl -fsS http://127.0.0.1:3756/health/ready", check=False)
        run(client, "docker ps --filter name=nvidia-router-app-1 --format '{{.Image}}\t{{.Status}}'")
        run(client, f"docker logs --tail 30 nvidia-router-app-1 2>&1 | tail -30", check=False)
        print(f"\ndeployed {image} at {release}\nrollback: {previous_image} at {current}", flush=True)
    finally:
        client.close()
        archive.unlink(missing_ok=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
