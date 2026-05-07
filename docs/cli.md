# `aa26-connector` CLI

The CLI is the fastest path from "I want to write a connector" to "I have a working connector running locally". Five subcommands.

```
aa26-connector new <name> --lang=python|bash [--dir=PATH]
aa26-connector validate [PATH]
aa26-connector lint [PATH] [--strict]
aa26-connector test [PATH] [flags]
aa26-connector package [--out=FILE]
```

It's a single static Go binary with no dependencies. On the prototype VM it's already at `/usr/local/bin/aa26-connector`. To install on your own machine:

```bash
git clone https://github.com/gman247/aa26-connector-cli.git
cd aa26-connector-cli
make install   # go install → ~/go/bin/aa26-connector
```

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

## `aa26-connector lint`

Statically scans connector source for known HTTP/JSON anti-patterns. Every rule maps to a real production failure that already happened to a real connector — the goal is "this exact bug class can't ship again."

```bash
$ aa26-connector lint
✓ lint clean
```

```bash
$ aa26-connector lint
⚠ [R001/warn] connector.py:60: json.load(resp) on a urllib response will crash on 204/empty bodies (e.g. cold-start /v1/checkpoint). Check resp.status == 204 first or read+strip the body.
    return json.load(resp)
0 error(s), 1 warning(s)
```

Rule reference:

| Rule | Languages | Severity | Catches |
|------|-----------|----------|---------|
| R001 | Python    | warn     | `json.load(resp)` on a `urllib` response |
| R002 | Python    | warn     | `requests.<verb>(...).json()` without 204 guard |
| R003 | Go        | warn     | `json.NewDecoder(resp.Body).Decode` without `StatusNoContent` check |
| R004 | TS / JS   | warn     | `await response.json()` after `fetch()` without status check |
| R005 | All       | error    | Sidecar URL drift (port ≠ 8089, `runtime:` host) |

`--strict` promotes warnings to failures. The lint also runs automatically at the start of `aa26-connector test`; pass `--skip-lint` to bypass.

## `aa26-connector test`

Runs your connector image against an in-process sidecar emulator (or, with `--real-runtime`, the production runtime container). The emulator stands in for the real runtime sidecar — same HTTP API, same endpoints — but everything stays on your laptop. No cluster needed.

```bash
$ aa26-connector test
✓ connector.yaml is valid
→ lint clean
→ emulator listening on 127.0.0.1:8089
→ running localhost/snowflake:dev (op=access_scan, function_type=access-scan)
worker | starting scan...
worker | wrote 97 findings

─── summary ───────────────────────────────────────
  invocations:  1
  findings:     97 (valid: 97)
  progress:     2
  log events:   0 (errors: 0)
  status:       completed
  coverage:
    ✓ GET  /v1/invocation     1x
    · GET  /v1/checkpoint     0x
    · POST /v1/checkpoint     0x
    ✓ POST /v1/findings       2x
    ✓ POST /v1/progress       2x
    · POST /v1/log            0x
    · GET  /v1/control        0x
    · POST /v1/process        0x
    ✓ POST /v1/complete       1x
  result:       ✓ pass
```

The emulator captures everything your connector POSTs and prints a summary. The coverage table shows which sidecar endpoints your worker actually hit — a cheap way to spot untested code paths. On first-call failures (worker exits non-zero before `/v1/complete`), the summary includes a **forensic block** with the last sidecar interaction, recent worker output, and a heuristic root-cause hint.

Required: a built image matching `spec.image.repository:spec.image.tag` from your manifest. Tag defaults to `dev` if omitted in the manifest.

### Flags

| Flag                | Effect |
|---------------------|--------|
| `--fixture=FILE`    | Use `FILE` instead of `./test-fixture.yaml` (missing file is fine) |
| `--non-interactive` | Don't prompt; missing required field → fail. Use this in CI. |
| `--save-fixture`    | After resolving prompts, write the answers back to the fixture file. |
| `--keep-going`      | Don't exit non-zero on expectation failure (lets you read the diff). |
| `--probe-contract`  | Run the worker through every contract probe scenario (cold start, warm start, sidecar errors). Slower; cleanest pre-PR check. |
| `--real-runtime`    | Run the production runtime container instead of the in-process emulator. Eliminates emulator drift. |
| `--runtime-image=X` | Override the runtime image used by `--real-runtime`. |
| `--strict`          | Fail on lint warnings, not just errors. |
| `--skip-lint`       | Skip the pre-flight lint pass. |

See [test-harness.md](test-harness.md) for the full reference: fixture format, fault-injection via `emulator.responses`, the probe matrix, and real-runtime mode.

## `aa26-connector package`

Bundles the current directory into a deployable `.tar.gz` for upload to AA26 via the **+ Add New Source** UI.

```bash
$ aa26-connector package
✓ wrote my-connector-0.1.0.tar.gz (12.4 MB)
```

Validates the manifest first, runs `docker save` on the image declared in `spec.image`, and emits `<name>-<version>.tar.gz`. See [uploading.md](uploading.md) for the full flow.

## Typical author loop

```bash
# 1. Scaffold
aa26-connector new my-connector --lang=python
cd my-connector

# 2. Edit connector.py for your data store
# (open in editor, write the actual logic)

# 3. Loop fast
aa26-connector validate
aa26-connector lint
docker build -t localhost/my-connector:dev .
aa26-connector test --save-fixture       # first run; saves your prompt answers

# 4. Once happy, run the full contract probe
aa26-connector test --probe-contract --strict --non-interactive

# 5. Ship
aa26-connector package
# → upload my-connector-0.1.0.tar.gz at /connector-upload/
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
- **`test` lint flags `R001` / `R002`**: your sidecar client calls `json.load(resp)` or `.json()` on a response that may be 204 with an empty body. The most common offender is `GET /v1/checkpoint` — the production sidecar returns 204 when no checkpoint exists. Check `resp.status == 204` (urllib) or `resp.status_code != 204` (requests) before parsing.
- **`--probe-contract` shows `cold-start ✗ / warm-start ✓`**: the connector handles the resume path but crashes when no checkpoint exists. Same root cause as R001/R002 above.
