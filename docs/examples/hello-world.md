# Example — Hello world (40 lines of bash)

The simplest end-to-end connector. Scans a directory, emits one finding per file, and proves the framework actually does what the docs say. Mirror of [demo-shell in the prototype tree](https://github.com/netwrix-dev/connector-prototype/tree/main/sample-connectors/demo-shell).

## The whole thing

`connector.yaml`:

```yaml
apiVersion: connectors.netwrix.io/v1
kind: Connector
metadata:
  name: hello-world
  displayName: Hello World
  version: 0.1.0
  vendor: local
spec:
  image:
    repository: localhost/hello-world
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

`run.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail
S=http://127.0.0.1:8089

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
    curl -sf -X POST -d '{"status":"failed","summary":{"reason":"unknown op"}}' "$S/v1/complete"
    exit 2
    ;;
esac
```

`Dockerfile`:

```dockerfile
FROM alpine:3.20
RUN apk add --no-cache bash curl jq findutils coreutils
COPY run.sh /app/run.sh
RUN chmod +x /app/run.sh
USER 65532
ENTRYPOINT ["/app/run.sh"]
```

## Annotated walkthrough

```bash
inv=$(curl -sf "$S/v1/invocation")
```

That single call is how the connector finds out what it's been asked to do. The sidecar synthesizes the invocation from a ConfigMap that AA26 prepared. You don't have to know how — just that this is the entry point.

```bash
op=$(jq -r '.operation' <<<"$inv")
```

Two operations matter for this connector: `test_connection` (the user clicked Test in the UI) and `scan` (run the actual scan). Anything else is an error.

```bash
find "$root" -type f -print0 | while IFS= read -r -d '' f; do
  jq -nc ... | ...
done | curl ... /v1/findings
```

The whole loop emits NDJSON to stdout, then a single curl POSTs the entire stream to the sidecar. The sidecar accepts any number of newline-separated objects in one POST. For a handful of findings this works. For a million you'd batch in chunks of 50–500 lines and POST each batch — see [demo-fs](databricks.md) (the Python sample) for that pattern.

```bash
curl -sf -X POST -d '{"status":"completed"}' "$S/v1/complete"
```

This is the signal that you're done. The sidecar exits shortly after responding, and the Job ends. Without this, your container exits but the sidecar keeps running until the Job's `activeDeadlineSeconds` kicks in — bad UX.

## Run it

Build, side-load, drop the manifest:

```bash
docker build -t localhost/hello-world:dev .
sudo docker save localhost/hello-world:dev | sudo k3s ctr images import -

sudo mkdir -p /var/lib/aa26/connectors/hello-world
sudo cp connector.yaml /var/lib/aa26/connectors/hello-world/
```

Within 30s the registry picks it up. AA26's UI shows "Hello World" in the connector picker. Configure a Source with `rootPath: /etc`, run a scan, and watch the Scan Executions tab.

## What you skipped

This example doesn't:

- Take credentials (the `credentials.schema` is empty)
- Emit progress (no calls to `/v1/progress`)
- Save checkpoints (the scan's quick enough to not need pause/resume)
- Poll for control signals (`/v1/control`)
- Honor retries on the framework's behalf (it doesn't have to — the sidecar does that)

For a connector you'd actually ship, you'd add those — but each is opt-in. Start with the 40 lines, ship something working, then add what you need.
