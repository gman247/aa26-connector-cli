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

The bread-and-butter for access scans. These findings are routed by the runtime to the `permissions` ClickHouse table (not `entities`). Your connector manifest must declare a sourceType with `ingestion.target: permissions` to provide the column mapping — see [manifest-reference.md §spec.sourceTypes](manifest-reference.md#specSourceTypes).

```json
{
  "schemaVersion": "1.0",
  "kind": "finding",
  "executionId": "0e7c3a98-...",
  "occurredAt": "2026-05-05T20:00:00Z",
  "type": "access_grant",
  "sourceType": "DropboxPermission",
  "subject": {
    "kind": "user",
    "id": "dropbox:user:dbid:abc123",
    "displayName": "Alice Anderson",
    "canonicalEmail": "alice@corp.com"
  },
  "object": {
    "kind": "file",
    "id": "id:abc123def456"
  },
  "permissions": {
    "aceType": "Allow",
    "memberRole": "Member",
    "readAllowed": true,
    "writeAllowed": true,
    "deleteAllowed": true,
    "manageAllowed": false,
    "adminAllowed": false,
    "listAllowed": true
  }
}
```

**Key fields for access_grant:**

| Field | Required | Notes |
|---|---|---|
| `subject.canonicalEmail` | ✅ | The runtime derives `principalId` (a stable UUIDv5) from this field. Without it, the permissions row can't be linked to identities. Use the user's login email; for groups without an email address use a stable synthetic ID like `dropbox:group:<group-id>`. |
| `subject.id` | ✅ | Source-system stable ID. Becomes `permissionGrantId` when mapped. Must be unique per (file, principal) pair. |
| `object.id` | ✅ | The runtime derives `targetEntityId` (UUIDv5) from this field to link the permission row to the entity in the `entities` table. Must match the `object.id` used in the corresponding `object_metadata` finding. |
| `permissions.*` | recommended | Boolean permission flags (`readAllowed`, `writeAllowed`, `deleteAllowed`, `manageAllowed`, `adminAllowed`, `listAllowed`) plus `aceType` (`Allow`/`Deny`) and `memberRole` (`Owner`/`Member`/`Guest`). Map these via the sourceType's `ingestion.mapping`. |
| `sourceType` | ✅ (if manifest has multiple sourceTypes) | Must match the name of a sourceType in your manifest that has `ingestion.target: permissions`. Use a distinct name from your object_metadata sourceType — sharing a name causes the runtime to route both finding types through the same mapping and clobber entity rows. |

Subject `kind` values you'll commonly use: `user`, `group`, `service_account`. Object `kind` is whatever the object is in your source: `file`, `folder`, `table`, `site`. Use IDs that are stable within the source.

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

## Permission principals lookup table

When the runtime sidecar processes an `access_grant` finding it writes two rows simultaneously:

1. **`permissions`** — one row per (file, principal, permission) triple
2. **`permission_principals`** — one row per unique principal, keyed by `principalId`

`permission_principals` stores `canonicalEmail` and `displayName` exactly once, regardless of how many files that principal has access to. Join it against `permissions` to resolve identity without repeating the email on every permission row:

```sql
SELECT
    p.targetEntityId,
    e.name              AS file_name,
    e.pathSegment       AS file_path,
    pp.canonicalEmail,
    pp.displayName,
    p.aceType,
    p.memberRole,
    p.readAllowed,
    p.writeAllowed,
    p.deleteAllowed,
    p.manageAllowed
FROM access_analyzer.permissions AS p
LEFT JOIN access_analyzer.entities AS e
    ON e.entityId = p.targetEntityId
LEFT JOIN access_analyzer.permission_principals AS pp
    ON pp.principalId = p.principalId
ORDER BY p.crawlTimestampUtc DESC
LIMIT 100
```

`permission_principals` schema:

| Column | Type | Notes |
|---|---|---|
| `principalId` | UUID | UUIDv5(framework-namespace, canonicalEmail). Stable across all connectors — the same email always maps to the same UUID. Primary key. |
| `canonicalEmail` | String | The login email used to derive `principalId`. For synthetic identities (e.g. "anyone with link"), a stable namespaced string like `google-workspace:anyone:with-link`. |
| `displayName` | String | Human-readable name from the source system at scan time. May vary across scans. |
| `crawlTimestampUtc` | DateTime64(6) | ReplacingMergeTree version column — latest scan wins on dedup. |

## Common mistakes

- **Forgetting `schemaVersion`**: it's required. The sidecar 400s.
- **Putting raw secrets / PII in `evidence.raw` or `predicate.sample`**: redact. The data goes to ClickHouse and shows up in UI search.
- **Using free-form `type` strings without the `custom:` prefix**: rejected. Built-in types are an enum; custom types must namespace.
- **Sending one giant POST instead of streaming**: NDJSON is for streaming. Send one finding per line, flush in batches of 50–500. The sidecar handles backpressure.
- **Setting your own `executionId`**: don't. Read it from `/v1/invocation`. The sidecar tags everything; if your `executionId` doesn't match the invocation, the sidecar trusts the invocation and your finding ends up in the wrong scan.
- **Omitting `subject.canonicalEmail` on access_grant findings**: the runtime derives `principalId` from this field using UUIDv5. Without it the permission row lands with a null principal and can't be linked to an identity in the UI. Always include the user's login email.
- **Sharing a `sourceType` name between `object_metadata` and `access_grant`**: the runtime uses the sourceType name to look up the ingestion mapping. If both finding types use the same name, the runtime routes them through the same mapping (targeting either `entities` or `permissions`, not both) and one set of findings is dropped or mis-stored.
- **Declaring `access_scan` in capabilities but no `ingestion.target: permissions` sourceType**: access_grant findings will be dropped by the runtime with "unsupported finding type" until a permissions-targeted sourceType is declared. `aa26-connector lint` rule **R008** catches this.
