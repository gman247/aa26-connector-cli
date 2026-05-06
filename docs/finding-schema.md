# Finding schema

Every event your connector emits goes through one envelope. There are three event kinds (`finding`, `progress`, `log`), and within `finding` there are three built-in archetypes (`access_grant`, `object_metadata`, `sensitive_match`) plus a `custom:` extension namespace. The full JSON Schema is at `https://connectors.netwrix.io/schema/finding.schema.json`.

## The envelope

Every event has these fields:

```json
{
  "schemaVersion": "1.0",
  "kind": "finding",                          // "finding" | "progress" | "log"
  "executionId": "0e7c3a98-...",
  "sourceId":    "5b1d22f0-...",              // optional but recommended
  "occurredAt":  "2026-05-05T20:00:00Z"
}
```

`executionId` you read from `GET /v1/invocation`. The sidecar tags every event you emit so AA26 knows which scan execution to attach it to.

## Finding archetypes

### `access_grant` — "this principal has this permission on this object"

The bread-and-butter for access scans.

```json
{
  "schemaVersion": "1.0",
  "kind": "finding",
  "executionId": "0e7c3a98-...",
  "occurredAt": "2026-05-05T20:00:00Z",
  "type": "access_grant",
  "subject": {
    "kind": "principal",
    "id": "user:alice@corp.com",
    "displayName": "Alice Anderson"
  },
  "object": {
    "kind": "table",
    "id": "snowflake://DEMO.PUBLIC.CUSTOMERS",
    "displayName": "DEMO.PUBLIC.CUSTOMERS",
    "url": "https://abc.snowflakecomputing.com/console#/data/databases/DEMO/schemas/PUBLIC/table/CUSTOMERS"
  },
  "predicate": {
    "permission": "select",
    "inherited": true,
    "via": "role:DEMO_READER"
  },
  "evidence": {
    "raw": {
      "grantee": "DEMO_READER",
      "grant_type": "ROLE",
      "with_grant_option": false
    }
  },
  "labels": {
    "sensitivity": "internal"
  }
}
```

Subject `kind` values you'll commonly use: `principal`, `group`, `service_account`, `role`. Object `kind` is whatever makes sense for your data store: `file`, `folder`, `table`, `view`, `bucket`, `mailbox`, `message`, `site`, `row`. Use IDs that are stable within the source — they're how the UI groups and links findings.

### `object_metadata` — "this object exists"

Inventory without permissions. Used for sync scans.

```json
{
  "schemaVersion": "1.0",
  "kind": "finding",
  "executionId": "0e7c3a98-...",
  "occurredAt": "2026-05-05T20:00:00Z",
  "type": "object_metadata",
  "object": {
    "kind": "file",
    "id": "/etc/hosts",
    "path": "/etc/hosts",
    "size": 200
  },
  "evidence": {
    "raw": {
      "mode": "-rw-r--r--",
      "mtime": "2026-04-12T09:23:00Z",
      "owner": "root"
    }
  }
}
```

Skip `subject` and `predicate` for this archetype.

### `sensitive_match` — "this object contains sensitive content"

Classification result.

```json
{
  "schemaVersion": "1.0",
  "kind": "finding",
  "executionId": "0e7c3a98-...",
  "occurredAt": "2026-05-05T20:00:00Z",
  "type": "sensitive_match",
  "object": {
    "kind": "table",
    "id": "snowflake://DEMO.PUBLIC.CUSTOMERS",
    "displayName": "DEMO.PUBLIC.CUSTOMERS"
  },
  "predicate": {
    "match": "PII.email",
    "classifier": "regex/email_v3",
    "region": { "column": "EMAIL_ADDRESS", "rows_sampled": 100, "rows_matched": 96 },
    "sample": "a***@example.com"
  },
  "labels": { "sensitivity": "high" }
}
```

`match` is the classification key; `classifier` is which classifier produced it. Avoid putting raw sensitive content in `sample` — redact, hash, or omit.

### `custom:<your.namespace>`

For finding types that don't fit the three above, declare them in your manifest:

```yaml
permissions:
  findingTypes:
    - access_grant
    - "custom:snowflake.share_grant"
    - "custom:snowflake.row_access_policy"
```

Then emit them with the matching `type`:

```json
{
  "type": "custom:snowflake.share_grant",
  "object":  {"kind": "share", "id": "..."},
  "subject": {"kind": "account", "id": "..."},
  "predicate": {"shareLevel": "READ_ONLY"}
}
```

Only types listed in `findingTypes` are accepted. The sidecar 400s anything else with the offending type in the response.

## Progress events

```json
{
  "schemaVersion": "1.0",
  "kind": "progress",
  "executionId": "0e7c3a98-...",
  "occurredAt": "2026-05-05T20:00:00Z",
  "processed": 1500,
  "total": 12000,
  "message": "scanning catalog DEMO"
}
```

Drives the progress bar in the Scan Executions tab. Emit periodically — every 5–10 seconds is plenty.

## Log events

```json
{
  "schemaVersion": "1.0",
  "kind": "log",
  "executionId": "0e7c3a98-...",
  "occurredAt": "2026-05-05T20:00:00Z",
  "level": "warn",
  "message": "encountered locked table",
  "attributes": {
    "table": "DEMO.PUBLIC.LOCKED_THING",
    "retry_count": 3
  }
}
```

`level` ∈ `debug | info | warn | error`. `attributes` is freeform string→anything; UI surfaces it as filter facets.

## Common mistakes

- **Forgetting `schemaVersion`**: it's required. The sidecar 400s.
- **Putting raw secrets / PII in `evidence.raw` or `predicate.sample`**: redact. The data goes to ClickHouse and shows up in UI search.
- **Using free-form `type` strings without the `custom:` prefix**: rejected. Built-in types are an enum; custom types must namespace.
- **Sending one giant POST instead of streaming**: NDJSON is for streaming. Send one finding per line, flush in batches of 50–500. The sidecar handles backpressure.
- **Setting your own `executionId`**: don't. Read it from `/v1/invocation`. The sidecar tags everything; if your `executionId` doesn't match the invocation, the sidecar trusts the invocation and your finding ends up in the wrong scan.
