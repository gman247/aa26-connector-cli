# `aa26-connector` CLI

The CLI is the fastest path from "I want to write a connector" to "I have a working connector running locally". Three subcommands.

```
aa26-connector new <name> --lang=python|bash [--dir=PATH]
aa26-connector validate [PATH]
aa26-connector test [PATH] --root=DIR [--op=scan|test_connection]
```

It's a single static Go binary with no dependencies. On the prototype VM it's already at `/usr/local/bin/aa26-connector`. On your machine, `cd /home/azureuser/connector-prototype/cli && make install` (sudo).

## `aa26-connector new`

Scaffolds a working connector skeleton in a fresh directory.

```bash
aa26-connector new snowflake --lang=python
```

Creates:

```
snowflake/
  connector.yaml      # manifest with sane defaults — name, displayName, version
  connector.py        # Python skeleton (or run.sh for --lang=bash)
  Dockerfile          # ready to build
  README.md           # boilerplate, points back at these docs
```

Pick `--lang=bash` if you want the simplest possible starting point — bash + `curl` + `jq` is enough to ship a connector. Pick `--lang=python` if you'll need OAuth/SDKs/JSON-heavy code (almost everyone past hello-world).

## `aa26-connector validate`

Validates `connector.yaml` (or any path you pass) against the published JSON Schema. Run early, run often:

```bash
$ aa26-connector validate
✓ connector.yaml is valid
```

```bash
$ aa26-connector validate
✗ invalid:
jsonschema: '/apiVersion' does not validate with .../connector.schema.json#/properties/apiVersion/const: value must be "connectors.netwrix.io/v1"
```

The error always points at the offending JSON path and explains what was expected. Same validator the registry runs at admission time, so "validate ok here" means "Ready in registry there."

## `aa26-connector test`

Runs your connector image against an in-process sidecar emulator. The emulator stands in for the real runtime sidecar — same HTTP API, same endpoints — but everything stays on your laptop. No cluster needed.

```bash
$ aa26-connector test --root=/etc --op=scan
✓ connector.yaml is valid
→ running localhost/snowflake:dev (op=scan, root=/etc)
  findings batch ( 50  events)
  findings batch ( 47  events)

✓ test complete
  invocations:  1
  findings:     97
  progress:     2
  logs:         0
  status:       completed
```

The emulator captures everything your connector POSTs and prints a summary. If your connector forgot to call `/v1/complete`, the test fails with a pointer to the runtime contract. If it crashes, the container's stdout/stderr is shown.

Required: a built image matching `spec.image.repository:spec.image.tag` from your manifest. Tag defaults to `dev` if omitted in the manifest.

`--op=test_connection` runs the test_connection path; `--op=scan` (default) runs scan.

## Typical author loop

```bash
# 1. Scaffold
aa26-connector new my-connector --lang=python
cd my-connector

# 2. Edit connector.py for your data store
# (open in editor, write the actual logic)

# 3. Loop fast
aa26-connector validate
docker build -t localhost/my-connector:dev .
aa26-connector test --root=/some/test/dir

# 4. When happy, ship
sudo cp -r ../my-connector /var/lib/aa26/connectors/my-connector
# registry picks it up; check status:
kubectl -n connector-prototype port-forward svc/connector-registry 8090:8090 &
curl -s http://localhost:8090/status | jq '.connectors[] | select(.name=="my-connector")'
```

## What the CLI doesn't do (yet)

- **Push to a registry.** Use plain `docker push` — see [publishing](publishing.md).
- **Generate non-trivial schemas.** The skeleton's `connector.yaml` covers the simplest case (one source field, no credentials). Edit it to declare what your connector actually needs.
- **Mock external dependencies.** The emulator doesn't know your data store exists. If your connector talks to Snowflake, you still need a real Snowflake account or your own mock.
- **Build the image for you.** You run `docker build`. The CLI doesn't touch your Dockerfile.

## Troubleshooting

- **"docker without sudo failed — retrying with sudo"**: harmless. Your user isn't in the `docker` group on this host. Run `sudo usermod -aG docker $USER && newgrp docker` to fix permanently, or ignore it.
- **"could not find connector.schema.json"**: you're running the CLI from a non-standard location. Set `CONNECTOR_SCHEMA=/path/to/connector.schema.json`.
- **`test` exits with "connector did not POST /v1/complete"**: your handler runs, emits findings, then exits without telling the framework it's done. Add a `POST /v1/complete` at the end. See [runtime contract](runtime-contract.md#post-v1complete).
