# aa26-connector-cli

CLI for authoring **AA26 (Netwrix DSPM) connectors** — the things that let AA26 scan a new kind of data store. Scaffold a connector skeleton, validate the manifest, statically lint the source for known footguns, run the worker locally against an in-process sidecar emulator, and package the bundle for upload.

A single static Go binary with no dependencies. Linux only for `test` (uses `docker run --network=host`).

## Quick start

```bash
# Install
git clone https://github.com/gman247/aa26-connector-cli.git
cd aa26-connector-cli
make install   # → ~/go/bin/aa26-connector

# Scaffold + run
aa26-connector new my-connector --lang=python
cd my-connector
docker build -t localhost/my-connector:dev .
aa26-connector test --save-fixture
```

## Subcommands

| Command                              | What it does |
|--------------------------------------|--------------|
| `aa26-connector new <name>`          | Scaffold a connector skeleton (`connector.yaml`, handler, Dockerfile). |
| `aa26-connector validate [PATH]`     | Validate `connector.yaml` against the published JSON Schema. |
| `aa26-connector lint [PATH]`         | Static-lint connector source for known HTTP/JSON anti-patterns (json.load on 204, sidecar URL drift, etc). |
| `aa26-connector test [PATH] [flags]` | Run the worker locally against the sidecar contract. Auto-runs lint first. |
| `aa26-connector package [--out=F]`   | Bundle the directory into a deployable `.tar.gz` for upload. |

Run `aa26-connector --help` for the full flag reference.

## Test harness highlights

The `test` subcommand is the fast inner-loop tool. Beyond running the worker, it:

- **Lints** the connector source for known footguns (cold-start `json.load(204)`, sidecar URL drift, etc.) before any docker work.
- **Validates** every `/v1/findings` envelope against the published schema. Schema violations are unconditional failures.
- **Reports per-endpoint coverage** so you can see which sidecar paths your worker actually hit. Pair with `expect.requiredEndpoints` in the fixture to lock coverage in CI.
- **Forensic block** on first-call failures — when the worker dies before `/v1/complete`, the summary points at the last sidecar interaction, the trailing worker output, and a heuristic root-cause hint.
- **Fault injection** via `emulator.responses` in the fixture — write deliberate negative tests for 204/500/edge responses without touching connector code.
- **Contract probe (`--probe-contract`)** runs the worker through a curated matrix of edge cases (cold start, warm start, sidecar errors). Cleanest pre-PR check.
- **Real-runtime mode (`--real-runtime`)** runs the production runtime container instead of the in-process emulator, eliminating emulator-vs-production drift.

## Documentation

- **[Quickstart](docs/quickstart.md)** — write a working connector in 15 minutes
- **[CLI reference](docs/cli.md)** — every subcommand, every flag
- **[Test harness](docs/test-harness.md)** — fixture format, lint rules, probe matrix, real-runtime mode
- **[Manifest reference](docs/manifest-reference.md)** — every field of `connector.yaml`
- **[Runtime contract](docs/runtime-contract.md)** — the HTTP API your container talks to
- **[Finding schema](docs/finding-schema.md)** — what your output looks like
- **[Publishing](docs/publishing.md)** — pushing images, side-loading into k3s
- **[Uploading](docs/uploading.md)** — the **+ Add New Source** flow

## Build / test

```bash
make build   # vet + test + build → bin/aa26-connector
make test    # vet + test only
make install # go install → $GOPATH/bin
```

Requirements: Go 1.18+, `docker` (only for `test` and `package`).

## License

See [LICENSE](LICENSE).
