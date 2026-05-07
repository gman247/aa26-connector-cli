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
2. **Lint** the connector source for known footguns (json.load on a
   204 response, sidecar URL drift, etc.). Warnings are advisory by
   default; pair with `--strict` to fail on any. See the [lint
   reference](#lint-pre-flight) below.
3. **Verify** the worker image exists locally (no auto-pull — your
   `:dev` tag is whatever you last built).
4. **Resolve** the connection block: anything in `test-fixture.yaml`'s
   `connection:` section wins; missing fields are prompted for unless
   `--non-interactive` is set.
5. **Start** the emulator on `127.0.0.1:8089` (or, with `--real-runtime`,
   start the production runtime container instead).
6. **Run** the worker via `docker run --rm --network=host` with the
   same env vars core-api would set: `REQUEST_DATA`, `FUNCTION_TYPE`,
   `SCAN_EXECUTION_ID`, `SCAN_ID`, `SOURCE_ID`, `SOURCE_TYPE`,
   `SOURCE_VERSION`, plus anything from the fixture's `env:` block.
7. **Stream** stdout/stderr live (prefixed `worker | `) while keeping
   the trailing 200 lines for the forensic block.
8. **Validate** every NDJSON line on `/v1/findings` against
   `finding.schema.json`. Schema violations are always failures,
   independent of fixture expectations.
9. **Evaluate** the fixture's `expect:` block — status, finding counts,
   types, no-error-logs, required-endpoint coverage.
10. Print a summary with sidecar-coverage table and (on first-call
    failures) a forensic block; exit non-zero if any expectation failed
    (unless `--keep-going`).

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
  requiredEndpoints:                    # fail if these were never called
    - /v1/checkpoint
emulator:                               # see "Fault injection" below
  responses:
    /v1/checkpoint:
      method: GET
      status: 200
      body: '{"cursor":"resume-token","seenCount":42}'
      headers:
        Content-Type: application/json
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

### `expect.requiredEndpoints`

A list of sidecar paths (or method-qualified `"GET /v1/path"` strings)
the worker must hit at least once. Fails the run if any are missing.
Use this to lock down code-path coverage in CI — for example,
`requiredEndpoints: [/v1/checkpoint]` ensures every fixture you ship
exercises the resume path.

### `emulator.responses`

Per-endpoint response overrides for fault injection. Each entry replaces
the emulator's default reply for one path. The override applies to all
methods unless `method` is set. Use this to write deliberate negative
tests:

```yaml
emulator:
  responses:
    /v1/log:
      method: POST
      status: 500
      body: "internal error"
```

The `body` field is a raw string; supply your own JSON if you want
JSON. Headers are merged on top of the emulator's defaults — set
`Content-Type` here to override.

## Flags

| Flag                | Effect                                                                |
|---------------------|-----------------------------------------------------------------------|
| `--fixture=FILE`    | Use `FILE` instead of `./test-fixture.yaml` (missing file is fine)    |
| `--non-interactive` | Don't prompt; missing required field → fail. Use this in CI.          |
| `--save-fixture`    | After resolving prompts, write the answers back to the fixture file.  |
| `--keep-going`      | Don't exit non-zero on expectation failure (lets you read the diff).  |
| `--probe-contract`  | Run the worker through every contract probe scenario (cold start,     |
|                     | warm start, sidecar errors). Slower; cleanest pre-PR check.           |
| `--real-runtime`    | Run the production runtime container instead of the in-process        |
|                     | emulator. Eliminates emulator-vs-production drift.                    |
| `--runtime-image=X` | Override the runtime image used by `--real-runtime`. Defaults to      |
|                     | `docker.io/connector-prototype/runtime:dev`.                          |
| `--strict`          | Fail on lint warnings, not just errors.                               |
| `--skip-lint`       | Skip the pre-flight lint pass.                                        |

## Lint pre-flight

`aa26-connector test` runs the static linter against the connector
source before any docker work happens. The goal is to catch a small
class of HTTP/JSON anti-patterns at author time rather than at runtime
in production. Every rule maps to a real bug that has fired against a
real connector.

| Rule | Languages | Severity | What it catches |
|------|-----------|----------|-----------------|
| R001 | Python    | warn     | `json.load(resp)` on a `urllib` response — crashes on 204/empty bodies (e.g. cold-start `GET /v1/checkpoint`). |
| R002 | Python    | warn     | `requests.get(...).json()` without a `status_code != 204` guard — same bug class via `requests`/`httpx`. |
| R003 | Go        | warn     | `json.NewDecoder(resp.Body).Decode(...)` without a `StatusNoContent` check. |
| R004 | TS / JS   | warn     | `await response.json()` after `fetch()` without a `response.status !== 204` check. |
| R005 | All       | error    | Sidecar URL drift — the runtime listens on `127.0.0.1:8089`; anything else (8088, 8090, `runtime:` host) won't reach the harness or production. |

Warnings don't fail the run by default — first-time authors aren't
blocked by stylistic rules. Errors always do. Promote warnings to
failures with `--strict`. Bypass entirely with `--skip-lint`. Run
standalone via `aa26-connector lint [PATH]`.

The lint deliberately stays small. Generic linters (pylint,
golangci-lint, eslint) catch the broader space; this one only commits
to bug classes that have actually broken connectors.

## Sidecar coverage report

The post-run summary includes a coverage table — every documented
sidecar endpoint, with the number of times the worker called it.
Unrecognized paths (typos, leftover hardcoded URLs) are surfaced too,
so coverage isn't silently mis-attributed.

```
coverage:
  · GET  /v1/invocation     0x
  ✓ GET  /v1/checkpoint     1x
  · POST /v1/checkpoint     0x
  · POST /v1/findings       0x      ← smoking gun: worker died before findings
  · POST /v1/progress       0x
  ✓ POST /v1/log            2x
  ✓ POST /v1/complete       1x
  ? GET /healthz            1x (unrecognized path)
```

If a connector never touches `/v1/checkpoint` in your fixture but
production calls it on every cold start, that's a glaring "you have an
untested code path." Add `expect.requiredEndpoints` once you've seen
the coverage table to hard-fail future regressions.

## Forensic block

When the worker exits non-zero **before** posting `/v1/complete`, the
summary includes a forensic block pinpointing what went wrong:

```
─── forensic ──────────────────────────────────────
  last sidecar call: GET /v1/checkpoint → 204 at T+0.31s
  worker exit: 1
  image: localhost/web-crawler:dev
  likely cause: worker likely crashed parsing the empty 204 response
                from GET /v1/checkpoint (common: json.load on empty body).
  worker output (last 20 lines):
    | web-crawler: op=access-scan
    | Traceback (most recent call last):
    | ...
    | json.decoder.JSONDecodeError: Expecting value: line 1 column 1 (char 0)
```

Heuristics intentionally stay narrow — the block leans on what the
harness directly observed (last call, exit code, recent output). For
graceful failures where the worker reports `status=failed` via
`/v1/complete` itself, the coverage table and `log events` count tell
the same story without the forensic block.

## Contract probe (`--probe-contract`)

Runs the worker through a curated matrix of edge cases the production
sidecar can produce:

| Scenario            | Description |
|---------------------|-------------|
| `cold-start`        | `GET /v1/checkpoint` returns 204 with empty body (no prior run). |
| `warm-start`        | `GET /v1/checkpoint` returns 200 with a saved-state JSON object. |
| `control-empty-200` | `GET /v1/control` returns 200 with `{}` (the production no-signal default). |
| `log-rejected`      | `POST /v1/log` returns 500 — connector should keep working without logs. |
| `progress-rejected` | `POST /v1/progress` returns 500 — connector should keep working without progress. |

Each scenario re-runs the worker with the relevant fault injected.
Output is a per-scenario pass/fail matrix:

```
─── probe summary ─────────────────────────────────
  ✗ cold-start           status=failed
      got status="failed", want one of [completed]
  ✓ warm-start           status=completed
  ✗ control-empty-200    status=failed
      got status="failed", want one of [completed]
  ✓ log-rejected         status=failed
  ✓ progress-rejected    status=failed
```

The matrix is the cleanest pre-PR check: a connector that passes every
scenario handles every documented response shape without crashing. The
juxtaposition of `cold-start` failing and `warm-start` passing
typically pinpoints the bug class within seconds.

## Real-runtime mode (`--real-runtime`)

Instead of the in-process emulator, run the **actual
`connector-prototype/runtime` sidecar container** on the same network
namespace as the worker. Eliminates emulator-vs-production drift
entirely: if a connector works under `--real-runtime` locally, the
guarantee that it works in cluster is much stronger.

```bash
aa26-connector test --real-runtime
aa26-connector test --real-runtime --runtime-image=docker.io/connector-prototype/runtime:dev
```

How it works:

```
┌──── runtime container ────┐    ┌──── worker container ────┐
│  127.0.0.1:8089 listener  │    │  hardcoded localhost:8089│
└─────────────┬─────────────┘    └─────────────┬────────────┘
              │ same network namespace via                  │
              └──── docker --network=container:<runtime> ───┘
```

The harness mounts an invocation file into the runtime container so
`/v1/invocation` returns the same payload the in-process emulator
would, points `FINDINGS_OUTPUT_FILE` at a host file, and reads the
findings back for schema validation after the worker exits.

Trade-offs vs. the emulator:

- **Pro:** zero drift from production. Same binary, same code paths.
- **Pro:** future runtime changes (Phase-2 long-poll, secret mapping)
  light up automatically without harness updates.
- **Con:** requires the runtime image locally (`docker pull` or
  `docker build`). The emulator is self-contained.
- **Con:** loses fault-injection — `emulator.responses` is ignored
  because the real runtime owns the responses.
- **Con:** terminal status comes from the worker's docker exit code,
  not `/v1/complete` (the runtime swallows complete before the harness
  sees it).

Use the emulator for fast inner-loop iteration; reach for
`--real-runtime` when you suspect emulator drift or before opening a
PR.

## What the emulator does

| Endpoint              | Behavior                                                                     |
|-----------------------|------------------------------------------------------------------------------|
| `GET /v1/invocation`  | Returns op + connection block from the fixture.                              |
| `POST /v1/findings`   | NDJSON; each line validated against `finding.schema.json`; counts + raw kept.|
| `POST /v1/progress`   | 200 with `{}` body (matches the production runtime — see runtime-contract.md). |
| `POST /v1/log`        | Body parsed as a `kind=log` envelope; level + message recorded.              |
| `GET /v1/control`     | 2-second sleep then `{}`; mimics long-poll without burning cpu.              |
| `GET /v1/checkpoint`  | 204 with empty body (matches production "no saved checkpoint").              |
| `POST /v1/checkpoint` | 204; not persisted (real sidecar uses Redis).                                |
| `POST /v1/process`    | `{"status":"queued"}`; not actually dispatched.                              |
| `POST /v1/complete`   | Records terminal status; the harness exits shortly after.                    |
| `GET /healthz`        | `ok\n`                                                                       |

Per-endpoint defaults can be overridden via `emulator.responses` in
the fixture (see [Fault injection](#emulatorresponses)).

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

### Platform behavior

- **Linux:** the harness runs the worker with `docker run --network=host`,
  which puts it on the same loopback as the in-process emulator. The
  emulator binds `127.0.0.1:8089`. No environment variables required.
- **macOS / Windows:** `--network=host` is a no-op, so the harness drops
  it. The emulator binds `0.0.0.0:8089` instead, and the worker is given
  `SIDECAR_URL=http://host.docker.internal:8089` — the special hostname
  Docker Desktop resolves to the host. Connectors that hardcode
  `localhost:8089` should also accept `SIDECAR_URL` as an override (the
  scaffolded skeleton does).
- **`--real-runtime` mode** uses `docker run --network=container:<runtime>`
  to share a network namespace between the runtime and worker containers.
  This works on every platform Docker supports — both containers see the
  same loopback regardless of host OS.

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

## CI recipe

A reasonable CI invocation looks like:

```bash
aa26-connector test \
  --non-interactive \
  --strict \
  --probe-contract \
  --fixture=ci-fixture.yaml
```

`--non-interactive` ensures no prompt blocks the build. `--strict`
fails on lint warnings. `--probe-contract` exercises every documented
response shape. The combination guarantees that what works in CI works
against any production sidecar response shape.
