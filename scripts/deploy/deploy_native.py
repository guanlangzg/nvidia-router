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


def run(client: paramiko.SSHClient, command: str, timeout: int = 60) -> str:
    print(f"$ {command}", flush=True)
    _, stdout, stderr = client.exec_command(command, timeout=timeout)
    out = stdout.read().decode().strip()
    err = stderr.read().decode().strip()
    if out:
        print(out)
    if err:
        print("ERR:", err)
    return out


def main() -> int:
    bin_path = build_binary()
    client = connect()
    try:
        run(client, f"mkdir -p {TARGET_DIR}/data {TARGET_DIR}/backups")

        print("Uploading binary via SFTP...", flush=True)
        sftp = client.open_sftp()
        sftp.put(str(bin_path), f"{TARGET_BIN}.new")
        sftp.chmod(f"{TARGET_BIN}.new", 0o755)
        sftp.close()

        run(client, f"mv {TARGET_BIN}.new {TARGET_BIN}")
        run(client, "systemctl restart nvidia-router", timeout=120)
        time.sleep(2)

        run(client, "systemctl is-active nvidia-router")
        run(client, "curl -s http://127.0.0.1:3756/health/live")
        run(client, "curl -s http://127.0.0.1:3756/health/ready")
        print("\nNative binary deployed and verified successfully!", flush=True)
    finally:
        client.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
