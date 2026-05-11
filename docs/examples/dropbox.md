# Dropbox connector — OAuth2 walkthrough

A complete, working sketch of a Dropbox SDS connector that uses the
framework's OAuth2 engine. Read this end-to-end before you start writing
your own OAuth-using connector — most of the design decisions here
generalize to any provider.

## What we're building

A connector that:
1. Authenticates to Dropbox via OAuth2.
2. Enumerates files in the user's Dropbox account.
3. Downloads each file's content and emits it as `object_metadata`
   findings with a `content` field, which the framework's classifier
   relay forwards to Evidence AI for sensitive-data detection.

The OAuth dance is handled entirely by the framework. The connector's
code is plain "call the Dropbox API with a Bearer token" — no redirect
handling, no token exchange, no refresh logic.

## File layout

```
dropbox-connector/
├── connector.yaml          # the manifest
├── Dockerfile
└── src/
    └── scan.py             # ~80 lines of Python
```

## `connector.yaml`

```yaml
apiVersion: connectors.netwrix.io/v1
kind: Connector
metadata:
  name: dropbox
  displayName: Dropbox
  version: 0.1.0
  vendor: community
  description: |
    Scans files in a user's Dropbox account for sensitive data.
    Uses OAuth2 (per-user authorization).

spec:
  image:
    repository: ghcr.io/<your-org>/dropbox-connector
    tag: 0.1.0

  capabilities:
    scanTypes: [sensitive_data_scan]
    operations: [test_connection, scan]

  source:
    schema:
      type: object
      properties:
        rootPath:
          type: string
          x-display: "Root path"
          description: "Folder to scan (use empty string for whole Dropbox)."
          default: ""

  auth:
    methods:
      - type: oauth2
        provider: dropbox
        displayName: "Sign in with Dropbox"
        authorizationUrl: "https://www.dropbox.com/oauth2/authorize"
        tokenUrl:         "https://api.dropboxapi.com/oauth2/token"
        revocationUrl:    "https://api.dropboxapi.com/2/auth/token/revoke"
        scopes:
          - account_info.read
          - files.metadata.read
          - sharing.read
          - files.content.read
        pkce: true
        extraAuthParams:
          token_access_type: offline    # tells Dropbox to issue a refresh token

  sourceTypes:
    - name: DropboxFile
      domain: Artifact
      ingestion:
        target: entities
        mapping:
          name:           [object, name]
          sourceSystemId: [object, id]
          size_bytes:     [object, size]
          modified_time:  [object, server_modified]
```

The `auth.methods[].type=oauth2` block is everything you need for OAuth.
No callback URL, no client_id/client_secret — the deployment operator
sets those at install time per the customer runbook.

## `src/scan.py`

```python
#!/usr/bin/env python3
"""Dropbox SDS connector — minimal walkthrough."""

import json
import sys
import time

import requests

SIDECAR = "http://127.0.0.1:8089"


def get_creds() -> dict:
    """Read the current OAuth credentials from the framework sidecar.

    The sidecar handles refresh on its end — we just ask, we get a fresh
    token. If the token has died permanently (refresh-token revoked), the
    sidecar returns 503 with X-OAuth-State: needs_reauthorization and
    we fail the scan with a clear marker so the operator can re-connect.
    """
    r = requests.get(f"{SIDECAR}/v1/credentials", timeout=10)
    if r.status_code == 503 and r.headers.get("X-OAuth-State") == "needs_reauthorization":
        post_complete(status="failed",
                      summary={"error": "OAUTH_REAUTHORIZATION_REQUIRED",
                               "detail": r.json().get("detail", "")})
        sys.exit(0)  # sidecar /v1/complete already ran; clean exit
    if r.status_code == 404:
        # No OAuth configured. This connector requires it — fail loud.
        post_complete(status="failed",
                      summary={"error": "OAUTH_NOT_CONFIGURED",
                               "detail": "This connector requires OAuth2 setup; see /lab/evidence"})
        sys.exit(0)
    r.raise_for_status()
    return r.json()


def dropbox_call(method: str, endpoint: str, creds: dict, **kw):
    """Wrap a Dropbox API call with 401-on-expiry retry semantics."""
    url = f"https://api.dropboxapi.com/2/{endpoint}"
    headers = kw.pop("headers", {})
    headers["Authorization"] = f"Bearer {creds['access_token']}"
    resp = requests.request(method, url, headers=headers, **kw)
    if resp.status_code == 401:
        # Token expired mid-scan. Ask the sidecar — it refreshes
        # transparently and gives us a fresh one.
        new_creds = get_creds()
        creds.update(new_creds)  # mutate so caller sees the new token
        headers["Authorization"] = f"Bearer {creds['access_token']}"
        resp = requests.request(method, url, headers=headers, **kw)
    resp.raise_for_status()
    return resp


def post_finding(envelope: dict) -> None:
    """Stream one finding to the sidecar as NDJSON."""
    body = json.dumps(envelope) + "\n"
    r = requests.post(f"{SIDECAR}/v1/findings",
                      headers={"Content-Type": "application/x-ndjson"},
                      data=body, timeout=10)
    r.raise_for_status()


def post_progress(processed: int) -> None:
    requests.post(f"{SIDECAR}/v1/progress",
                  json={"processed": processed}, timeout=5)


def post_complete(status: str, summary: dict | None = None) -> None:
    requests.post(f"{SIDECAR}/v1/complete",
                  json={"status": status, "summary": summary or {}}, timeout=10)


def main():
    # 1. Pull the invocation. Tells us what to scan and (for OAuth)
    #    that the user has authorized.
    inv = requests.get(f"{SIDECAR}/v1/invocation", timeout=5).json()
    root = inv.get("source", {}).get("rootPath", "")
    execution_id = inv["executionId"]

    # 2. Pull OAuth credentials. Proactive — sidecar's cache makes this
    #    cheap and it pre-warms the refresh path.
    creds = get_creds()

    # 3. Walk Dropbox. files/list_folder with recursive=true is the
    #    canonical way; pagination via list_folder/continue.
    processed = 0
    cursor = None
    while True:
        if cursor is None:
            resp = dropbox_call("POST", "files/list_folder", creds,
                                json={"path": root, "recursive": True,
                                      "include_media_info": False,
                                      "include_deleted": False})
        else:
            resp = dropbox_call("POST", "files/list_folder/continue", creds,
                                json={"cursor": cursor})
        data = resp.json()

        for entry in data.get("entries", []):
            if entry.get(".tag") != "file":
                continue
            try:
                # Pull file content for the framework's classifier relay.
                # Dropbox uses a separate content-host for downloads.
                api_arg = json.dumps({"path": entry["id"]})
                dl = requests.post(
                    "https://content.dropboxapi.com/2/files/download",
                    headers={"Authorization": f"Bearer {creds['access_token']}",
                             "Dropbox-API-Arg": api_arg},
                    timeout=30,
                )
                if dl.status_code == 401:
                    creds.update(get_creds())
                    dl = requests.post(
                        "https://content.dropboxapi.com/2/files/download",
                        headers={"Authorization": f"Bearer {creds['access_token']}",
                                 "Dropbox-API-Arg": api_arg},
                        timeout=30,
                    )
                dl.raise_for_status()
                content = dl.content.decode("utf-8", errors="replace")
            except Exception as e:
                # Don't fail the whole scan on one bad file.
                content = ""
                err_detail = str(e)
            else:
                err_detail = None

            post_finding({
                "schemaVersion": "1.0",
                "kind": "finding",
                "executionId": execution_id,
                "sourceId": inv.get("sourceId"),
                "occurredAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "type": "object_metadata",
                "sourceType": "DropboxFile",
                "object": {
                    "kind": "file",
                    "id": entry["id"],
                    "name": entry["name"],
                    "path": entry["path_display"],
                    "size": entry.get("size", 0),
                    "server_modified": entry.get("server_modified"),
                    "sourceSystemIdType": "dropbox_id",
                },
                "content": content,
                "evidence": {"raw": {"error": err_detail} if err_detail else {}},
            })
            processed += 1
            if processed % 50 == 0:
                post_progress(processed)

        if not data.get("has_more"):
            break
        cursor = data["cursor"]

    post_progress(processed)
    post_complete(status="completed",
                  summary={"files": processed, "errors": 0})


if __name__ == "__main__":
    main()
```

That's the whole connector. **Zero OAuth code.** The only OAuth-related
calls are `get_creds()` and the 401-retry helper around `dropbox_call`.

## `Dockerfile`

```dockerfile
FROM python:3.12-slim
WORKDIR /app
RUN pip install --no-cache-dir requests
COPY src/scan.py .
ENTRYPOINT ["python", "scan.py"]
```

## Local testing

> **Status today:** token-injection is the supported test path. Mock
> provider + browser modes ship in a follow-up CLI release alongside
> the framework's OAuth2 implementation.

Token-injection: grab a real Dropbox token (e.g. by manually completing
the OAuth flow at https://www.dropbox.com/developers/apps in your dev
app) and hand it to the harness:

```bash
aa26-connector test ./dropbox-connector \
  --auth oauth2 \
  --oauth-token 'sl.B1234567890.your-real-or-test-token' \
  --invocation '{"operation":"scan","executionId":"test","sourceId":"t",
                "source":{"rootPath":""},"scan":{}}'
```

This skips the consent UI entirely and exercises the connector code
path (including the `GET /v1/credentials` sidecar contract) against
real Dropbox APIs. Good enough for the connector author's iteration loop.

Real-browser mode (planned):

```bash
export DROPBOX_OAUTH_CLIENT_ID=<dev-app-key>
export DROPBOX_OAUTH_CLIENT_SECRET=<dev-app-secret>
aa26-connector test ./dropbox-connector --browser
```

This opens a browser, walks you through the Dropbox consent at the
dev app, then runs your connector with the resulting credentials.

## Customer install runbook

This goes in your connector's README, not here. The customer reads it
once when they first install the connector. The framework handles
everything else.

```markdown
## Register a Dropbox app for your AA deployment

Before connecting Dropbox sources, your AA installation needs a Dropbox
OAuth app. This is a one-time setup per AA deployment.

1. Sign in to https://www.dropbox.com/developers/apps and click "Create
   app".
2. API: "Scoped access". Type: "Full Dropbox" (or "App folder" if you
   want to scope scanning to a single folder).
3. Name: "Access Analyzer — <your organization>". This shows on the
   consent screen for your users.
4. Under "Permissions", grant these scopes (default-off):
   - account_info.read
   - files.metadata.read
   - sharing.read
   - files.content.read
   Click "Submit" to save.
5. Under "OAuth 2", add this redirect URI:
   https://<your-aa-hostname>/api/v1/oauth/callback/dropbox
6. Copy the "App key" (client_id) and click "Show" next to "App secret"
   to reveal the secret.
7. On the AA deployment, add these env vars to the core-api Deployment:
   - DROPBOX_OAUTH_CLIENT_ID=<App key from step 6>
   - DROPBOX_OAUTH_CLIENT_SECRET=<App secret from step 6>
8. Restart core-api:
   `kubectl rollout restart deployment/core-api -n access-analyzer`
9. In the AA webapp, create a new "Dropbox" source. Click "Sign in with
   Dropbox" and authorize with the Dropbox account that owns the files
   you want to scan.

Done. Subsequent sources of type "Dropbox" can be added without repeating
this setup — the OAuth app is shared across all Dropbox sources in this
AA deployment.
```

## What to remember

- **You never write OAuth code.** Token exchange, refresh, storage,
  revocation — all the framework.
- **`get_creds()` is your one OAuth interaction.** Call it at scan start;
  call it again on a 401 from the provider. The sidecar handles the rest.
- **Handle 503 + `X-OAuth-State: needs_reauthorization` gracefully.** The
  framework already transitioned the source to a re-auth state; your job
  is just to fail the scan cleanly so the operator notices.
- **The customer registers the OAuth app, not you.** Document it in your
  README. The framework supplies a per-deployment callback URL pattern.
- **PKCE is on by default and Just Works.** Don't disable it unless your
  provider genuinely doesn't support it (rare in 2026).

For the full spec see **[../oauth2.md](../oauth2.md)** and the
authoritative architecture doc at `dspm:connector-framework/docs/oauth2-v1.md`.
