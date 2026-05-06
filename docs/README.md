# Connector author docs

Everything you need to write a connector for the AA26 plug-and-play framework. **If you're an AI agent that's been handed this repo, start here**: read the pages in this order and you'll have full context.

## Read in this order

1. **[index.md](index.md)** — what a connector is, the diagram of how it fits together, and why the framework exists
2. **[quickstart.md](quickstart.md)** — write a working bash connector in 15 minutes
3. **[manifest-reference.md](manifest-reference.md)** — every field of `connector.yaml` with examples
4. **[runtime-contract.md](runtime-contract.md)** — the HTTP API your container talks to (the *only* Netwrix-specific surface you have to learn)
5. **[finding-schema.md](finding-schema.md)** — what your output looks like (NDJSON envelope, three archetypes, custom extensions)
6. **[cli.md](cli.md)** — the `aa26-connector` CLI reference (`new`, `validate`, `test`, `package`)
7. **[uploading.md](uploading.md)** — how to ship the bundle (web UI or curl)
8. **[publishing.md](publishing.md)** — the alternative filesystem-drop path for operators with shell access

## Examples

Two complete, working connectors live alongside the docs:

- **[examples/hello-world.md](examples/hello-world.md)** — annotated walkthrough of a 40-line bash connector
- **[examples/databricks.md](examples/databricks.md)** — Python skeleton for a more realistic OAuth-authenticated data store

Source code for both is in this repo at [`../examples/connectors/demo-fs/`](../examples/connectors/demo-fs/) (Python) and [`../examples/connectors/demo-shell/`](../examples/connectors/demo-shell/) (bash).

## Schema

The canonical JSON Schema for `connector.yaml` is at [`../schema/connector.schema.json`](../schema/connector.schema.json). It's also embedded into the `aa26-connector` binary at build time, so `aa26-connector validate` doesn't need the file separately.

## When you get stuck

The runtime contract docs page is the most load-bearing reference — every connector talks to the same nine HTTP endpoints. If your connector is misbehaving, re-read [runtime-contract.md](runtime-contract.md) and trace through what your code actually sends. Most connector bugs are "I forgot to POST `/v1/complete`" or "I'm sending the wrong `executionId`."
