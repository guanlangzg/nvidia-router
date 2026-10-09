"""Enable opencodefree/step-5-preview-free and trigger capability reprobe on hangzhou2-2."""

from __future__ import annotations

import json
import sys
import time
import urllib.error
import urllib.request

BASE = "http://127.0.0.1:3756"
admin_opener = None


def log(kind, **fields):
    fields["kind"] = kind
    print("R|" + json.dumps(fields, sort_keys=True, separators=(",", ":")), flush=True)


def call_admin(method, path, payload=None, timeout=60):
    data = None if payload is None else json.dumps(payload, separators=(",", ":")).encode()
    headers = {"Origin": BASE}
    if data is not None:
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(BASE + path, data=data, headers=headers, method=method)
    try:
        with admin_opener.open(request, timeout=timeout) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


def unwrap(body):
    try:
        value = json.loads(body)
    except Exception:
        return None
    if isinstance(value, dict) and "data" in value:
        return value["data"]
    return value


def main():
    global admin_opener
    admin_opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor())
    password = sys.stdin.readline().rstrip("\r\n")
    status, _ = call_admin("POST", "/admin/api/auth/login", {"username": "admin", "password": password})
    log("AUTH", step="login", status=status)
    if status != 200:
        return 2

    status, body = call_admin("GET", "/admin/api/models")
    catalog = unwrap(body) if status == 200 else []
    target = None
    for item in catalog:
        if item.get("public_id") == "opencodefree/step-5-preview-free":
            target = item
            break

    if not target:
        log("TARGET", found=False, error="model not found in catalog")
        return 1

    model_id = target["id"]
    log("TARGET", found=True, id=model_id, enabled=target.get("enabled"), tools_status=target.get("tools_status"))

    if not target.get("enabled"):
        patch_status, patch_body = call_admin("PATCH", f"/admin/api/models/{model_id}", {"enabled": True})
        log("ENABLE", status=patch_status, response=str(unwrap(patch_body))[:200])

    # Now trigger model test job for step-5-preview-free
    job_status, job_body = call_admin("POST", "/admin/api/model-test-jobs", {
        "model_ids": [model_id],
        "mode": "sequential",
    })
    job = unwrap(job_body) if job_status in (200, 201, 202) else {}
    job_id = job.get("id")
    log("REPROBE_JOB", status=job_status, job_id=job_id)

    if job_id:
        for _ in range(120):
            time.sleep(3)
            poll_status, poll_body = call_admin("GET", f"/admin/api/model-test-jobs/{job_id}")
            poll_job = unwrap(poll_body) if poll_status == 200 else {}
            if poll_job.get("status") in ("completed", "failed", "cancelled"):
                for result in poll_job.get("results") or []:
                    probe = result.get("probe") or {}
                    log("PROBE_RESULT",
                        public_id=result.get("public_id"),
                        status=result.get("status"),
                        error=result.get("error"),
                        base=probe.get("base"),
                        reasoning=probe.get("reasoning"),
                        tools=probe.get("tools"),
                        duration_ms=result.get("duration_ms"))
                break

    # Verify latest state
    status, body = call_admin("GET", "/admin/api/models")
    catalog = unwrap(body) if status == 200 else []
    for item in catalog:
        if item.get("public_id") == "opencodefree/step-5-preview-free":
            log("FINAL_MODEL_STATE",
                public_id=item.get("public_id"),
                enabled=item.get("enabled"),
                tools_status=item.get("tools_status"),
                supports_tools=item.get("supports_tools"),
                supports_reasoning=item.get("supports_reasoning"),
                reasoning_status=item.get("reasoning_status"),
                tools_verified_at=item.get("tools_verified_at"))
            break

    call_admin("POST", "/admin/api/auth/logout")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
