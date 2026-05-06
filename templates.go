// Templates rendered by `aa26-connector new`.
package main

const connectorYAMLTmpl = `apiVersion: connectors.netwrix.io/v1
kind: Connector
metadata:
  name: %s
  displayName: %s
  version: %s
  vendor: local
  description: |
    A new connector. Replace this with a useful description.
spec:
  image:
    repository: localhost/%s
    tag: dev
    pullPolicy: Never
  capabilities:
    operations: [test_connection, scan]
    scanTypes: [access_scan]
  credentials:
    schema:
      type: object
      properties: {}
  source:
    schema:
      type: object
      required: [rootPath]
      properties:
        rootPath:
          type: string
          x-display: "Root path"
  permissions:
    findingTypes: [object_metadata]
`

// Argument order for sprintf: name, displayName, version, image-segment.
// Caller passes name twice (once for displayName as a placeholder).

// — bash skeleton —

const bashHandler = `#!/usr/bin/env bash
# Connector skeleton — bash + curl + jq.
# Talks to the runtime sidecar at $SIDECAR_URL.
set -euo pipefail
S="${SIDECAR_URL:-http://127.0.0.1:8089}"

# 1. Read the invocation.
inv=$(curl -sf "$S/v1/invocation")
op=$(jq -r '.operation' <<<"$inv")
exec_id=$(jq -r '.executionId' <<<"$inv")
root=$(jq -r '.source.rootPath' <<<"$inv")

case "$op" in
  test_connection)
    if [[ -d "$root" ]]; then
      curl -sf -X POST -d '{"status":"completed"}' "$S/v1/complete"
    else
      curl -sf -X POST -d '{"status":"failed","summary":{"reason":"path missing"}}' "$S/v1/complete"
      exit 1
    fi
    ;;
  scan)
    find "$root" -type f -print0 | while IFS= read -r -d '' f; do
      jq -nc \
        --arg eid "$exec_id" --arg p "$f" --arg ts "$(date -u +%FT%TZ)" \
        --argjson sz "$(stat -c %s "$f" 2>/dev/null || echo 0)" '{
          schemaVersion:"1.0", kind:"finding",
          executionId:$eid, occurredAt:$ts,
          type:"object_metadata",
          object:{kind:"file", id:$p, path:$p, size:$sz}
        }'
    done | curl -sf -X POST -H 'Content-Type: application/x-ndjson' --data-binary @- "$S/v1/findings"
    curl -sf -X POST -d '{"status":"completed"}' "$S/v1/complete"
    ;;
  *)
    curl -sf -X POST -d '{"status":"failed"}' "$S/v1/complete"
    exit 2
    ;;
esac
`

const bashDockerfile = `FROM alpine:3.20
RUN apk add --no-cache bash curl jq findutils coreutils
COPY run.sh /app/run.sh
RUN chmod +x /app/run.sh
USER 65532
ENTRYPOINT ["/app/run.sh"]
`

// — python skeleton —

const pythonHandler = `#!/usr/bin/env python3
"""Connector skeleton — Python stdlib only.

Talks to the runtime sidecar at SIDECAR_URL (default localhost:8089).
"""
import json
import os
import sys
import time
import urllib.request

SIDECAR = os.environ.get("SIDECAR_URL", "http://127.0.0.1:8089")


def post(path, payload):
    body = json.dumps(payload).encode() if not isinstance(payload, (bytes, str)) else (
        payload.encode() if isinstance(payload, str) else payload
    )
    req = urllib.request.Request(
        SIDECAR + path,
        data=body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )
    return urllib.request.urlopen(req, timeout=30)


def get_invocation():
    return json.load(urllib.request.urlopen(SIDECAR + "/v1/invocation", timeout=10))


def now():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def emit_findings(findings):
    body = "\n".join(json.dumps(f) for f in findings).encode()
    req = urllib.request.Request(
        SIDECAR + "/v1/findings",
        data=body,
        method="POST",
        headers={"Content-Type": "application/x-ndjson"},
    )
    urllib.request.urlopen(req, timeout=60)


def run_test_connection(invocation):
    root = invocation["source"].get("rootPath", "")
    ok = bool(root and os.path.isdir(root))
    post("/v1/complete", {
        "status": "completed" if ok else "failed",
        "summary": {"reachable": ok},
    })
    return 0 if ok else 1


def run_scan(invocation):
    root = invocation["source"]["rootPath"]
    execution_id = invocation["executionId"]
    source_id = invocation.get("sourceId", "")

    findings = []
    for dirpath, _, filenames in os.walk(root):
        for name in filenames:
            full = os.path.join(dirpath, name)
            try:
                size = os.path.getsize(full)
            except OSError:
                continue
            findings.append({
                "schemaVersion": "1.0",
                "kind": "finding",
                "executionId": execution_id,
                "sourceId": source_id,
                "occurredAt": now(),
                "type": "object_metadata",
                "object": {"kind": "file", "id": full, "path": full, "size": size},
            })
            if len(findings) >= 100:
                emit_findings(findings)
                findings = []
    if findings:
        emit_findings(findings)
    post("/v1/complete", {"status": "completed"})
    return 0


def main():
    invocation = get_invocation()
    op = invocation.get("operation", "")
    print(f"connector op={op}", file=sys.stderr)
    if op == "test_connection":
        return run_test_connection(invocation)
    if op == "scan":
        return run_scan(invocation)
    post("/v1/complete", {"status": "failed", "summary": {"reason": f"unknown op: {op}"}})
    return 2


if __name__ == "__main__":
    sys.exit(main())
`

const pythonDockerfile = `FROM python:3.12-alpine
WORKDIR /app
COPY connector.py /app/connector.py
USER 65532
ENTRYPOINT ["python", "/app/connector.py"]
`

// — README —

const readmeTmpl = `# %s

A connector for AA26. Built with the %s skeleton.

## Build

` + "```" + `
docker build -t localhost/%s:dev .
` + "```" + `

## Validate

` + "```" + `
aa26-connector validate
` + "```" + `

## Test locally

` + "```" + `
aa26-connector test --root=/tmp
` + "```" + `

## Publish

See https://20.169.152.226.nip.io/connector-docs/publishing/ for the
filesystem-drop and OCI registry paths.
`
