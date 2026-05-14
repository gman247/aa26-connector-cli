# Quickstart — your first connector in 15 minutes

> **Implementation status:** This quickstart is fully working end-to-end. The connector you build here will run on the current cluster and write rows to ClickHouse.

You'll build a connector that scans a directory and emits one finding per file. It's the simplest thing that exercises the whole framework. We'll write it in **bash** to make the point that you don't need to learn anything Netwrix-specific to ship a connector.

You'll need:

- Access to a host running AA26 with the connector-prototype components deployed
- `docker` to build an image, or write access to a registry the cluster can pull from
- About 15 minutes

!!! tip "Use the CLI"
    There's a CLI that scaffolds, validates, and locally tests connectors without you having to type any of this by hand:
    ```
    aa26-connector new hello-fs --lang=bash
    cd hello-fs
    docker build -t localhost/hello-fs:dev .
    aa26-connector test --root=/etc
    ```
    See the [CLI reference](cli.md). The walkthrough below shows what's happening underneath so you understand the contract — the CLI is just an accelerator over the same files.

## 1. Write the manifest

Every connector starts with a `connector.yaml`. This file is the contract — it tells the framework what your connector is called, what it can do, and what fields the user has to fill in to configure it.

```yaml
# connector.yaml
apiVersion: connectors.netwrix.io/v1
kind: Connector
metadata:
  name: hello-fs
  displayName: Hello Filesystem
  version: 0.1.0
  vendor: local
spec:
  image:
    repository: localhost/hello-fs
    tag: dev
    pullPolicy: Never
  capabilities:
    operations: [test_connection, scan]
    scanTypes: [access_scan]
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
```

The `source.schema` block is JSON Schema. AA26's webapp renders it as a form on the **Create Source** screen — you don't have to ship UI code. `x-display` is an extension that becomes the field label.

## 2. Write the connector

Your container talks to a sidecar at `http://127.0.0.1:8089`. The sidecar tells you what scan to run, accepts your findings, and tells AA26 about them. That's the whole API.

```bash
#!/usr/bin/env bash
# run.sh — entire connector logic
set -euo pipefail

S=http://127.0.0.1:8089

# 1. Read what we're being asked to do.
inv=$(curl -sf "$S/v1/invocation")
op=$(jq -r '.operation' <<<"$inv")
exec_id=$(jq -r '.executionId' <<<"$inv")
root=$(jq -r '.source.rootPath' <<<"$inv")

# 2. test_connection: is the path reachable?
if [[ "$op" == "test_connection" ]]; then
  if [[ -d "$root" ]]; then
    curl -sf -X POST -d '{"status":"completed","summary":{"reachable":true}}' "$S/v1/complete"
  else
    curl -sf -X POST -d '{"status":"failed","summary":{"reason":"path missing"}}' "$S/v1/complete"
    exit 1
  fi
  exit 0
fi

# 3. scan: emit one finding per file as NDJSON.
find "$root" -type f -print0 | while IFS= read -r -d '' f; do
  jq -nc --arg eid "$exec_id" --arg p "$f" --arg ts "$(date -u +%FT%TZ)" \
     --argjson sz "$(stat -c %s "$f" 2>/dev/null || echo 0)" '{
       schemaVersion:"1.0", kind:"finding",
       executionId:$eid, occurredAt:$ts,
       type:"object_metadata",
       object:{kind:"file", id:$p, path:$p, size:$sz}
     }'
done | curl -sf -X POST -H 'Content-Type: application/x-ndjson' --data-binary @- "$S/v1/findings"

# 4. Tell the framework you're done.
curl -sf -X POST -d '{"status":"completed"}' "$S/v1/complete"
```

That's it. That is the entire connector.

## 3. Build the image

```dockerfile
# Dockerfile
FROM alpine:3.20
RUN apk add --no-cache bash curl jq findutils coreutils
COPY run.sh /app/run.sh
RUN chmod +x /app/run.sh
USER 65532
ENTRYPOINT ["/app/run.sh"]
```

```bash
docker build -t localhost/hello-fs:dev .
```

If you're running against a remote cluster, push to a registry the cluster can pull from and update `spec.image.repository` accordingly. On the prototype VM, [side-load the image](publishing.md#side-loading-into-k3s) into containerd.

## 4. Register the connector

Easiest path — upload the bundle through the UI:

```bash
aa26-connector package
# → wrote hello-fs-0.1.0.tar.gz
```

Open https://20.169.152.226.nip.io/connector-upload/ and drop the tarball on the upload zone. Within 5 seconds the **Installed connectors** table shows your new entry; within ~10 seconds AA26's webapp picker has a card for it.

If you'd rather skip the bundling and have shell access to the host, you can also drop the manifest directly into the watch directory:

```bash
sudo mkdir -p /var/lib/aa26/connectors/hello-fs
sudo cp connector.yaml /var/lib/aa26/connectors/hello-fs/
```

Either way, the registry picks it up within ~10 seconds. Confirm via:

```bash
curl -s https://20.169.152.226.nip.io/connector-upload/api/list | jq '.connectors[] | select(.name=="hello-fs")'
```

You should see `"state": "Ready"`. If you see `"InvalidManifest"`, the `reason` field tells you what's wrong. The most common one is forgetting `apiVersion: connectors.netwrix.io/v1`.

See **[uploading](uploading.md)** for the full upload flow including delete and trust model.

## 5. Run it

Open AA26's webapp. Click **Add Connector** — your "Hello Filesystem" should appear in the list. Create a Source against it with `rootPath` set to something readable, run a scan, and watch findings appear in the Scan Executions tab.

## What just happened

Five things, each of which the framework does so you didn't have to:

1. **Form rendering** — the `rootPath` field in your manifest became a webapp form field.
2. **Job scheduling** — when you clicked "Run", AA26 launched a Kubernetes Job with two containers: yours, plus the runtime sidecar.
3. **Invocation delivery** — the sidecar told you what to scan via `/v1/invocation`.
4. **Findings ingestion** — the sidecar took your NDJSON, tagged it with the execution ID, and forwarded it into the same pipeline that powers Scan Executions for every other connector.
5. **Completion** — when you POSTed `/v1/complete`, the sidecar exited and the Job ended cleanly.

## Where to next

- Read the **[manifest reference](manifest-reference.md)** when your connector grows credentials, multiple scan types, or capability metadata.
- Read the **[runtime contract](runtime-contract.md)** when you need progress reporting, checkpoints, or the long-poll control endpoint.
- Read the **[finding schema](finding-schema.md)** when you want to emit `access_grant` or `sensitive_match` findings, not just `object_metadata`.
- Read **[OAuth2](oauth2.md)** if your data source uses OAuth2 (Dropbox, Google Drive, M365, Salesforce, Slack, …). The framework owns the whole flow; your connector reads the token from one HTTP call.
- Read **[extraction](extraction.md)** if your connector needs to pull text out of files (PDF, DOCX, scanned images, etc.) — the framework ships a Tika+Tesseract sidecar so you don't bundle either yourself.
- Look at **[examples/databricks](examples/databricks.md)** for a more realistic skeleton, or **[examples/dropbox](examples/dropbox.md)** for an end-to-end OAuth2 walkthrough.
