#!/usr/bin/env python3
"""Demo filesystem connector — Phase 1 reference.

Walks `source.rootPath` from the invocation, emits one object_metadata
finding per file and one access_grant per (file, owner) pair. Posts
progress every 50 files. Designed as a copy-paste-able example for the
docs — uses only the Python stdlib so authors can fork it without
worrying about base images, package managers, or version pins.

Talks to the runtime sidecar at $SIDECAR_URL (default http://127.0.0.1:8089).
"""

import json
import os
import pwd
import grp
import stat
import sys
import time
import urllib.request
import urllib.error
import uuid

SIDECAR = os.environ.get("SIDECAR_URL", "http://127.0.0.1:8089")
PROGRESS_EVERY = 50


def post(path: str, payload, *, content_type: str = "application/json") -> int:
    """POST a JSON object (or NDJSON bytes) to the sidecar. Returns HTTP status."""
    if isinstance(payload, (dict, list)):
        body = json.dumps(payload).encode("utf-8")
    elif isinstance(payload, str):
        body = payload.encode("utf-8")
    else:
        body = payload
    req = urllib.request.Request(
        SIDECAR + path,
        data=body,
        method="POST",
        headers={"Content-Type": content_type},
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.status
    except urllib.error.HTTPError as e:
        # Surface the body in stderr — debugging an unhappy sidecar shouldn't
        # require teaching the user about exception chains.
        sys.stderr.write(f"sidecar {path} -> {e.code}: {e.read().decode()}\n")
        raise


def get_invocation() -> dict:
    with urllib.request.urlopen(SIDECAR + "/v1/invocation", timeout=10) as resp:
        return json.load(resp)


def now_iso() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def file_finding(execution_id: str, source_id: str, path: str) -> dict:
    """One object_metadata finding describing a regular file."""
    st = os.stat(path)
    return {
        "schemaVersion": "1.0",
        "kind": "finding",
        "executionId": execution_id,
        "sourceId": source_id,
        "occurredAt": now_iso(),
        "type": "object_metadata",
        "object": {
            "kind": "file",
            "id": path,
            "path": path,
            "size": st.st_size,
        },
        "evidence": {
            "raw": {
                "mode": stat.filemode(st.st_mode),
                "mtime": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(st.st_mtime)),
            }
        },
    }


def owner_finding(execution_id: str, source_id: str, path: str) -> dict:
    """One access_grant finding: 'this principal owns this file (read+write)'."""
    st = os.stat(path)
    try:
        owner = pwd.getpwuid(st.st_uid).pw_name
    except KeyError:
        owner = f"uid:{st.st_uid}"
    try:
        group = grp.getgrgid(st.st_gid).gr_name
    except KeyError:
        group = f"gid:{st.st_gid}"
    return {
        "schemaVersion": "1.0",
        "kind": "finding",
        "executionId": execution_id,
        "sourceId": source_id,
        "occurredAt": now_iso(),
        "type": "access_grant",
        "subject": {"kind": "principal", "id": f"posix:{owner}", "displayName": owner},
        "object": {"kind": "file", "id": path, "path": path},
        "predicate": {
            "permission": "owner",
            "via": f"group:{group}",
            "mode": oct(st.st_mode & 0o777),
        },
    }


def progress(execution_id: str, processed: int, message: str = "") -> None:
    post(
        "/v1/progress",
        {
            "schemaVersion": "1.0",
            "kind": "progress",
            "executionId": execution_id,
            "occurredAt": now_iso(),
            "processed": processed,
            "message": message,
        },
    )


def emit_findings(events) -> int:
    """Stream a list of finding dicts as NDJSON to /v1/findings."""
    body = "\n".join(json.dumps(e) for e in events).encode()
    req = urllib.request.Request(
        SIDECAR + "/v1/findings",
        data=body,
        method="POST",
        headers={"Content-Type": "application/x-ndjson"},
    )
    with urllib.request.urlopen(req, timeout=60) as resp:
        return resp.status


def run_test_connection(invocation: dict) -> None:
    """Return success if rootPath exists. Connectors typically validate creds
    + reachability here; a filesystem connector just stat's the path."""
    root = invocation["source"].get("rootPath", "")
    if not root or not os.path.isdir(root):
        post(
            "/v1/log",
            {
                "schemaVersion": "1.0",
                "kind": "log",
                "executionId": invocation["executionId"],
                "occurredAt": now_iso(),
                "level": "error",
                "message": f"rootPath {root!r} is not a directory",
            },
        )
        post(
            "/v1/complete",
            {"status": "failed", "summary": {"reason": "rootPath not found"}},
        )
        sys.exit(1)
    post(
        "/v1/log",
        {
            "schemaVersion": "1.0",
            "kind": "log",
            "executionId": invocation["executionId"],
            "occurredAt": now_iso(),
            "level": "info",
            "message": f"test_connection ok for {root}",
        },
    )
    post("/v1/complete", {"status": "completed", "summary": {"reachable": True}})


def run_scan(invocation: dict) -> None:
    """Walk rootPath, emit findings."""
    root = invocation["source"]["rootPath"]
    execution_id = invocation["executionId"]
    source_id = invocation.get("sourceId") or str(uuid.uuid4())

    processed = 0
    batch = []
    for dirpath, _dirnames, filenames in os.walk(root):
        for name in filenames:
            full = os.path.join(dirpath, name)
            try:
                batch.append(file_finding(execution_id, source_id, full))
                batch.append(owner_finding(execution_id, source_id, full))
            except (FileNotFoundError, PermissionError):
                continue
            processed += 1
            if processed % PROGRESS_EVERY == 0:
                emit_findings(batch)
                batch = []
                progress(execution_id, processed, f"scanned {processed} files")
    if batch:
        emit_findings(batch)
    progress(execution_id, processed, "done")
    post(
        "/v1/complete",
        {"status": "completed", "summary": {"files": processed}},
    )


def main() -> None:
    invocation = get_invocation()
    op = invocation.get("operation", "")
    print(f"demo-fs: invocation op={op} root={invocation['source'].get('rootPath')}",
          file=sys.stderr)
    if op == "test_connection":
        run_test_connection(invocation)
    elif op == "scan":
        run_scan(invocation)
    else:
        post(
            "/v1/complete",
            {"status": "failed", "summary": {"reason": f"unknown op: {op!r}"}},
        )
        sys.exit(2)


if __name__ == "__main__":
    main()
