#!/usr/bin/env python3
"""Run a local Python program on the domestic test host over SSH.

Usage:
    python remote_exec.py <program.py> [--stdin-env NVIDIA_ROUTER_ADMIN_PASSWORD] [--timeout 900]

The program source is base64-encoded and executed by the remote ``python3``.
Secrets are never passed as arguments: when ``--stdin-env`` is given the value
of that environment variable is written to the remote process stdin as a single
line, which keeps it out of the remote process table and out of any log.
"""

from __future__ import annotations

import argparse
import base64
import os
import shlex
import sys
from pathlib import Path

import paramiko

DEFAULT_SSH_CONFIG = (
    Path(__file__).resolve().parents[3] / "服务器管理" / "hangzhou2-2" / "ssh_config_local"
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("program", type=Path)
    parser.add_argument("--stdin-env", default="", help="env var whose value is piped to remote stdin")
    parser.add_argument("--timeout", type=float, default=900.0)
    parser.add_argument("--ssh-config", type=Path, default=DEFAULT_SSH_CONFIG)
    parser.add_argument("--host-alias", default="hangzhou2-2")
    parser.add_argument("--arg", action="append", default=[], help="KEY=VALUE placeholder substitution (__KEY__)")
    return parser.parse_args()


def connect(config_path: Path, alias: str) -> paramiko.SSHClient:
    config = paramiko.SSHConfig()
    with config_path.open(encoding="utf-8") as handle:
        config.parse(handle)
    host = config.lookup(alias)
    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    client.connect(
        host["hostname"],
        port=int(host.get("port", 22)),
        username=host.get("user", "root"),
        key_filename=host["identityfile"][0],
        timeout=20,
    )
    return client


def main() -> int:
    args = parse_args()
    program = args.program.read_text(encoding="utf-8")
    for item in args.arg:
        key, _, value = item.partition("=")
        program = program.replace("__%s__" % key, value)
    encoded = base64.b64encode(program.encode()).decode()
    command = "python3 -u -c " + shlex.quote(
        "import base64;exec(compile(base64.b64decode(%r),'<remote>','exec'))" % encoded
    )
    secret = ""
    if args.stdin_env:
        env_file = Path(__file__).resolve().parents[2] / ".env"
        if env_file.exists():
            for line in env_file.read_text(encoding="utf-8").splitlines():
                if line.startswith(f"{args.stdin_env}="):
                    secret = line.split("=", 1)[1].strip()
                    break
        if not secret:
            secret = os.environ.get(args.stdin_env, "")
        if not secret:
            raise SystemExit("%s is required" % args.stdin_env)
    client = connect(args.ssh_config, args.host_alias)
    try:
        stdin, stdout, stderr = client.exec_command(command, timeout=args.timeout)
        if args.stdin_env:
            stdin.write(secret + "\n")
        stdin.channel.shutdown_write()
        for line in iter(stdout.readline, ""):
            sys.stdout.write(line)
            sys.stdout.flush()
        error = stderr.read().decode("utf-8", "replace")
        status = stdout.channel.recv_exit_status()
    finally:
        client.close()
    if error:
        sys.stderr.write(error)
    return status


if __name__ == "__main__":
    raise SystemExit(main())
