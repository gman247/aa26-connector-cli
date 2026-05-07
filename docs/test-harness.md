# Test Harness (`aa26-connector test`)

The test harness runs your connector image **locally** against an
in-process emulator that matches the production runtime contract. No
cluster, no AA26 deploy, no round-trip — write a connector, build the
image, run `aa26-connector test`, see what happens.

```bash
docker build -t localhost/my-connector:dev .
aa26-connector test
```

The emulator listens on `127.0.0.1:8089` (the same port the production
sidecar uses), so connectors that hardcode `localhost:8089` work
unmodified locally and in cluster.

## Flow

1. **Validate** the manifest against `connector.schema.json`.
2. **Verify** the worker image exists locally (no auto-pull — your
   `:dev` tag is whatever you last built).
3. **Resolve** the connection block: anything in `test-fixture.yaml`'s
   `connection:` section wins; missing fields are prompted for unless
   `--non-interactive` is set.
4. **Start** the emulator on `127.0.0.1:8089`.
5. **Run** the worker via `docker run --rm --network=host` with the
   same env vars core-api would set: `REQUEST_DATA`, `FUNCTION_TYPE`,
   `SCAN_EXECUTION_ID`, `SCAN_ID`, `SOURCE_ID`, `SOURCE_TYPE`,
   `SOURCE_VERSION`, plus anything from the fixture's `env:` block.
6. **Stream** stdout/stderr live (prefixed `worker | `).
7. **Validate** every NDJSON line on `/v1/findings` against
   `finding.schema.json`. Schema violations are always failures,
   independent of fixture expectations.
8. **Evaluate** the fixture's `expect:` block — status, finding counts,
   types, no-error-logs.
9. Print a summary; exit non-zero if any expectation failed (unless
   `--keep-going`).

## Fixture format (`test-fixture.yaml`)

Every field is optional. The simplest possible fixture is a single line.

```yaml
op: access-scan
authMethod: basic                       # pin a spec.auth.methods[].type
connection:
  rootPath: /srv
  port: 8080
env:
  DEBUG: "1"
expect:
  status: completed                     # default
  noErrorLogs: true
  findings:
    minCount: 1
    maxCount: 10000
    types:
      - object_metadata
      - access_grant
```

### `op`

`FUNCTION_TYPE` env value the worker sees (dashed: `access-scan`,
`sensitive-data-scan`, `sync`, `test-connection`). Defaults to
`access-scan`. Mirrors `adapter_service.rb#build_scan_env` —
`scan_type.tr("_", "-")`. `/v1/invocation`'s `operation` field is the
underscored equivalent (`access_scan`, `test_connection`, …) which
matches what `runtime/main.go` returns in production.

### `authMethod`

When `spec.auth.methods` declares more than one method, pin which one
the harness uses by `type` (`basic`, `bearer`, `api_key`,
`service_account`, `none`). Without it: a single declared method is
auto-selected; multiple require either an interactive picker or this
field; `--non-interactive` prefers `none` if present, otherwise errors.

### `connection`

Whatever the worker should see in `REQUEST_DATA["connection"]` and as
`source` on `/v1/invocation`. Anything declared in
`spec.source.schema`, `spec.credentials.schema`, or
`spec.auth.methods[].fields` that you don't supply here is prompted for
when stdin is a tty.

### `env`

Extra environment variables on top of the framework defaults. Useful
for `DEBUG`, custom timeouts, third-party SDK knobs.

### `expect.status`

Terminal status the worker must POST to `/v1/complete`. Defaults to
`completed`. If the worker never POSTs `/v1/complete`, the harness
treats exit-code 0 as `completed` and non-zero as `failed`.

### `expect.findings`

`minCount` / `maxCount` / `types` — bounds on the validated findings
stream. Schema-invalid findings don't count toward `minCount` and
trigger an unconditional failure on top.

### `expect.noErrorLogs`

When true, fails the run if any `/v1/log` POST carried `level: error`.

## Flags

| Flag                | Effect                                                                |
|---------------------|-----------------------------------------------------------------------|
| `--fixture=FILE`    | Use `FILE` instead of `./test-fixture.yaml` (missing file is fine)    |
| `--non-interactive` | Don't prompt; missing required field → fail. Use this in CI.          |
| `--save-fixture`    | After resolving prompts, write the answers back to the fixture file. |
| `--keep-going`      | Don't exit non-zero on expectation failure (lets you read the diff). |

## What the emulator does

| Endpoint            | Behavior                                                                   |
|---------------------|----------------------------------------------------------------------------|
| `GET /v1/invocation`| Returns op + connection block from the fixture.                            |
| `POST /v1/findings` | NDJSON; each line validated against `finding.schema.json`; counts + raw kept. |
| `POST /v1/progress` | 200 with `{}` body (matches the production runtime — see runtime-contract.md). |
| `POST /v1/log`      | Body parsed as a `kind=log` envelope; level + message recorded.            |
| `GET /v1/control`   | 2-second sleep then `{}`; mimics long-poll without burning cpu.            |
| `POST /v1/checkpoint` | 204; not persisted (real sidecar uses Redis).                            |
| `POST /v1/process`  | `{"status":"queued"}`; not actually dispatched.                            |
| `POST /v1/complete` | Records terminal status; the harness exits shortly after.                  |
| `GET /healthz`      | `ok\n`                                                                     |

## What the emulator does NOT do

- **No data-ingestion forwarding.** `/v1/findings` does not POST to the
  AA26 data-ingestion service. Findings live in memory until the run
  ends and the summary prints. Add `--save-fixture` and a real cluster
  test once you've got the connector logic working.
- **No Redis-backed checkpoints.** Checkpoints are silently dropped.
- **No long-poll fan-out on `/v1/control`.** Always returns `{}` after
  ~2s; production reads from Redis Streams.
- **No secret-mapping injection.** Secrets in fixture `connection:`
  are passed through verbatim — the harness doesn't pretend to
  resolve OpenFaaS secret references.

## Constraints

### Linux-only (v1)

The harness uses `docker run --network=host` to put the worker on the
same loopback as the emulator. That doesn't work on macOS or Windows
(host-network is Linux-only).

**Workaround for now:** run the harness on a Linux dev box or VM.

**Plan for v2:** switch to a two-container approach:

```
emulator  → runs as a docker container (not in-process)
worker    → docker run --network=container:emulator
```

Both containers share the emulator container's loopback, so
`localhost:8089` resolves the same way it does in a real pod. No
host-network, works everywhere docker does.

### No `pullPolicy` honoring

The harness does not pull. If the image isn't already in your local
docker daemon, the run fails with a clear "build it first" message.
Connectors usually iterate on `:dev` tags built locally, so an
auto-pull would either 404 or grab a stale published copy.

### `--network=host` security

The worker can reach the dev host's network. That's intentional for
local dev — you want it to hit your local SQL Server / S3-compatible
emulator / whatever. Don't run untrusted images in your fixture.

## Reproducible runs

Once the prompts have walked you through the fields, save the answers:

```bash
aa26-connector test --save-fixture
```

That writes a fully populated `test-fixture.yaml` next to the manifest.
Check it into the connector source tree (minus secrets — pre-fill those
locally or load them from env via `${VAR}` interpolation, which the
harness will support in a future iteration).
