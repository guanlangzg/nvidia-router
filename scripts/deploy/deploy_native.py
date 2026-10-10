#!/usr/bin/env python3
"""Deploy the compiled native Linux binary to the domestic host under systemd.

Usage:
    python scripts/deploy/deploy_native.py
"""

from __future__ import annotations

import os
import subprocess
import time
from pathlib import Path

import paramiko

REPO = Path(__file__).resolve().parents[2]
SSH_CONFIG = REPO.parents[0] / "服务器管理" / "hangzhou2-2" / "ssh_config_local"
TARGET_DIR = "/opt/nvidia-router"
TARGET_BIN = f"{TARGET_DIR}/nvidia-router"


def build_binary() -> Path:
    bin_path = REPO / "tmp" / "nvidia-router"
    bin_path.parent.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    env["GOOS"] = "linux"
    env["GOARCH"] = "amd64"
    env["CGO_ENABLED"] = "0"
    print("Building Linux amd64 static binary...", flush=True)
    subprocess.run(
        ["go", "build", "-trimpath", "-ldflags=-s -w", "-o", str(bin_path), "./cmd/nvidia-router"],
        cwd=REPO,
        env=env,
        check=True,
    )
    print(f"Built {bin_path.name} ({bin_path.stat().st_size} bytes)", flush=True)
    return bin_path


def connect() -> paramiko.SSHClient:
    config = paramiko.SSHConfig()
    with SSH_CONFIG.open(encoding="utf-8") as handle:
        config.parse(handle)
    host = config.lookup("hangzhou2-2")
    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    client.connect(
        host["hostname"],
        port=int(host.get("port", 22)),
        username=host.get("user", "root"),
        key_filename=host["identityfile"][0],
        timeout=30,
    )
    return client


def run(client: paramiko.SSHClient, command: str, timeout: int = 60, check: bool = True) -> str:
    print(f"$ {command}", flush=True)
    stdin, stdout, stderr = client.exec_command(command, timeout=timeout)
    channel = stdout.channel
    out = stdout.read().decode().strip()
    err = stderr.read().decode().strip()
    status = channel.recv_exit_status()
    if out:
        print(out)
    if err:
        print("ERR:", err)
    if check and status != 0:
        raise RuntimeError(f"Command failed with exit status {status}: {command}")
    return out


def main() -> int:
    bin_path = build_binary()
    client = connect()
    ts = time.strftime("%Y%m%d-%H%M%S")
    backup_bin = f"{TARGET_DIR}/backups/nvidia-router.bak-{ts}"
    backup_db = f"{TARGET_DIR}/backups/router.db.bak-{ts}"
    try:
        run(client, f"mkdir -p {TARGET_DIR}/data {TARGET_DIR}/backups")

        # Snapshot existing binary and database before touching anything.
        print(f"Creating pre-deploy snapshots ({ts})...", flush=True)
        run(client, f"test -f {TARGET_BIN} && cp -a {TARGET_BIN} {backup_bin} || true")
        run(client, f"test -f {TARGET_DIR}/data/router.db && cp -a {TARGET_DIR}/data/router.db {backup_db} || true")

        print("Uploading binary via SFTP...", flush=True)
        sftp = client.open_sftp()
        sftp.put(str(bin_path), f"{TARGET_BIN}.new")
        sftp.chmod(f"{TARGET_BIN}.new", 0o755)
        sftp.close()

        # Atomic swap and restart.
        run(client, f"mv {TARGET_BIN}.new {TARGET_BIN}")
        run(client, "systemctl restart nvidia-router", timeout=120)
        time.sleep(2)

        # Verification gate.
        is_active = run(client, "systemctl is-active nvidia-router", check=False)
        live_code = run(client, "curl -s -m 5 -o /dev/null -w '%{http_code}' http://127.0.0.1:3756/health/live", check=False)
        ready_code = run(client, "curl -s -m 5 -o /dev/null -w '%{http_code}' http://127.0.0.1:3756/health/ready", check=False)

        if is_active != "active" or live_code != "200" or ready_code != "200":
            print(f"VERIFICATION FAILED (active={is_active}, live={live_code}, ready={ready_code})! Initiating rollback...", flush=True)
            run(client, f"test -f {backup_bin} && cp -a {backup_bin} {TARGET_BIN} && systemctl restart nvidia-router || true", check=False)
            raise RuntimeError(f"Deploy verification failed (live={live_code}, ready={ready_code}), rolled back to {backup_bin}")

        print(f"\nNative binary deployed and verified successfully! (live=200, ready=200, backup={backup_bin})", flush=True)
    finally:
        client.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
