# Example — Databricks (Python skeleton)

A more realistic connector. Uses OAuth M2M to authenticate, enumerates Unity Catalog tables, and emits one `access_grant` finding per `(principal, table, permission)` triple. Skeleton — the real one would also classify sample data and emit `sensitive_match` findings.

## Manifest

```yaml
apiVersion: connectors.netwrix.io/v1
kind: Connector
metadata:
  name: databricks
  displayName: Databricks
  version: 0.1.0
  vendor: local
  description: |
    Scan Databricks Unity Catalog for table inventory and access grants.
spec:
  image:
    repository: localhost/connector-prototype/databricks
    tag: dev
    pullPolicy: Never
  capabilities:
    scanTypes: [access_scan, sync]
    operations: [test_connection, discover, scan]
  credentials:
    schema:
      type: object
      required: [clientId, clientSecret]
      properties:
        clientId:
          type: string
          x-display: "Service Principal Client ID"
        clientSecret:
          type: string
          x-display: "Client Secret"
          x-secret: true
  source:
    schema:
      type: object
      required: [workspaceUrl]
      properties:
        workspaceUrl:
          type: string
          format: uri
          x-display: "Workspace URL"
          x-placeholder: "https://adb-1234567890.azuredatabricks.net"
        warehouseId:
          type: string
          x-display: "SQL Warehouse ID (for sample queries)"
        catalogFilter:
          type: array
          items: { type: string }
          x-display: "Catalog filter (regex list)"
          description: "Empty = all catalogs."
  runtime:
    network:
      egress:
        - "*.azuredatabricks.net"
        - "*.cloud.databricks.com"
        - "login.microsoftonline.com"
  permissions:
    findingTypes: [access_grant, object_metadata, sensitive_match]
```

## Connector

`connector.py`:

```python
#!/usr/bin/env python3
"""Databricks Unity Catalog connector — skeleton."""
import json, os, sys, time, urllib.request, urllib.parse

SIDECAR = os.environ.get("SIDECAR_URL", "http://127.0.0.1:8089")

def post_json(path, payload):
    req = urllib.request.Request(
        SIDECAR + path,
        data=json.dumps(payload).encode(),
        method="POST",
        headers={"Content-Type": "application/json"},
    )
    return urllib.request.urlopen(req, timeout=30).read()

def get_json(url, headers=None):
    req = urllib.request.Request(url, headers=headers or {})
    return json.loads(urllib.request.urlopen(req, timeout=30).read())

def now():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())

def get_invocation():
    return get_json(SIDECAR + "/v1/invocation")

def load_credentials(path):
    with open(path) as f:
        return json.load(f)

def get_oauth_token(workspace_url, client_id, client_secret):
    """Databricks OAuth M2M token endpoint."""
    body = urllib.parse.urlencode({
        "grant_type": "client_credentials",
        "scope": "all-apis",
    }).encode()
    auth = urllib.request.HTTPPasswordMgrWithDefaultRealm()
    auth.add_password(None, workspace_url, client_id, client_secret)
    handler = urllib.request.HTTPBasicAuthHandler(auth)
    opener = urllib.request.build_opener(handler)
    resp = opener.open(workspace_url + "/oidc/v1/token", body)
    return json.loads(resp.read())["access_token"]

def list_catalogs(workspace_url, token):
    return get_json(
        workspace_url + "/api/2.1/unity-catalog/catalogs",
        headers={"Authorization": f"Bearer {token}"},
    ).get("catalogs", [])

def list_tables(workspace_url, token, catalog, schema):
    qs = urllib.parse.urlencode({"catalog_name": catalog, "schema_name": schema})
    return get_json(
        workspace_url + "/api/2.1/unity-catalog/tables?" + qs,
        headers={"Authorization": f"Bearer {token}"},
    ).get("tables", [])

def list_grants(workspace_url, token, securable_type, full_name):
    qs = urllib.parse.urlencode({
        "securable_type": securable_type,
        "full_name": full_name,
    })
    return get_json(
        workspace_url + "/api/2.1/unity-catalog/permissions?" + qs,
        headers={"Authorization": f"Bearer {token}"},
    ).get("privilege_assignments", [])

def access_grant(execution_id, source_id, principal, table, privilege):
    return {
        "schemaVersion": "1.0",
        "kind": "finding",
        "executionId": execution_id,
        "sourceId": source_id,
        "occurredAt": now(),
        "type": "access_grant",
        "subject": {"kind": "principal", "id": f"databricks:{principal}", "displayName": principal},
        "object": {
            "kind": "table",
            "id": f"databricks://{table['catalog_name']}.{table['schema_name']}.{table['name']}",
            "displayName": f"{table['catalog_name']}.{table['schema_name']}.{table['name']}",
        },
        "predicate": {"permission": privilege.lower()},
    }

def emit_findings(events):
    body = "\n".join(json.dumps(e) for e in events).encode()
    req = urllib.request.Request(
        SIDECAR + "/v1/findings",
        data=body,
        method="POST",
        headers={"Content-Type": "application/x-ndjson"},
    )
    urllib.request.urlopen(req, timeout=60)

def run_test_connection(invocation, creds):
    workspace = invocation["source"]["workspaceUrl"].rstrip("/")
    try:
        token = get_oauth_token(workspace, creds["clientId"], creds["clientSecret"])
        list_catalogs(workspace, token)  # verifies the token actually works
        post_json("/v1/complete", {"status": "completed", "summary": {"reachable": True}})
    except Exception as e:
        post_json("/v1/complete", {"status": "failed", "summary": {"error": str(e)}})
        sys.exit(1)

def run_scan(invocation, creds):
    workspace = invocation["source"]["workspaceUrl"].rstrip("/")
    execution_id = invocation["executionId"]
    source_id = invocation.get("sourceId", "")

    token = get_oauth_token(workspace, creds["clientId"], creds["clientSecret"])
    catalogs = list_catalogs(workspace, token)

    findings = []
    for cat in catalogs:
        # in production: also list schemas, then tables in each schema.
        # skeleton: imagine schema enumeration is a one-call /api/.../schemas request.
        for schema in []:  # placeholder
            for tbl in list_tables(workspace, token, cat["name"], schema["name"]):
                full = f"{tbl['catalog_name']}.{tbl['schema_name']}.{tbl['name']}"
                for grant in list_grants(workspace, token, "TABLE", full):
                    for priv in grant.get("privileges", []):
                        findings.append(access_grant(
                            execution_id, source_id, grant["principal"], tbl, priv,
                        ))
                if len(findings) >= 200:
                    emit_findings(findings)
                    findings.clear()
    if findings:
        emit_findings(findings)
    post_json("/v1/complete", {"status": "completed", "summary": {"catalogs": len(catalogs)}})

def main():
    invocation = get_invocation()
    creds = load_credentials(invocation["credentialsPath"])
    op = invocation["operation"]
    if op == "test_connection":
        run_test_connection(invocation, creds)
    elif op == "scan":
        run_scan(invocation, creds)
    else:
        post_json("/v1/complete", {"status": "failed", "summary": {"reason": f"unknown op: {op}"}})
        sys.exit(2)

if __name__ == "__main__":
    main()
```

## What this skeleton skips (so you know what you're signing up for)

- **Pagination**: Databricks REST returns paginated results; production code follows `next_page_token` until exhausted.
- **Schema enumeration**: the inner loop is a placeholder. The real version calls `/api/2.1/unity-catalog/schemas` per catalog.
- **Sensitive-data scan**: this skeleton only emits `access_grant`. A real connector would also use the `warehouseId` to issue `SELECT ... LIMIT 100` queries against tables and stream rows through a classifier, emitting `sensitive_match` findings.
- **Checkpoints**: a long scan should `POST /v1/checkpoint` with the catalog/schema/table position so a pause-resume picks up where it left off.
- **Control polling**: a background thread should hit `GET /v1/control` and react to STOP/PAUSE.
- **Retries on 429**: Databricks rate-limits. Real code would back off on 429s. The sidecar doesn't retry to *external* services on your behalf — only to AA26 internals.

But: the runtime contract above (what to call, what to send) is **all** of what's framework-specific. Everything else is just Databricks-the-data-store.
