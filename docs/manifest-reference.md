# Manifest reference — `connector.yaml`

The manifest is the contract. Every connector ships one. The framework parses it, validates it against [the JSON Schema](https://connectors.netwrix.io/schema/connector.schema.json), and uses the result to drive the UI form, image discovery, capability gating, and (eventually) signature verification.

> **Implementation status:** Fields and features in this reference use the following tags:
> - `[implemented]` — works end-to-end on the current cluster
> - `[stored, not applied]` — accepted at upload and stored in the database, but the framework does not yet act on the value
> - `[stub]` — backend code exists but the UI or enforcement layer is incomplete
> - `[planned]` — reserved in the schema for a future release; no backend implementation exists yet
>
> Untagged fields are fully implemented.

Validate yours at any time:

```bash
aa26-connector validate ./connector.yaml
```

## Skeleton

```yaml
apiVersion: connectors.netwrix.io/v1     # required, exactly this string
kind: Connector                          # required, exactly this string

metadata:
  name: snowflake                        # required, lowercase slug
  displayName: Snowflake                 # required, shown in the UI picker
  version: 1.2.0                         # required, semver
  vendor: netwrix                        # optional: netwrix | community | local | <org>
  icon: ./assets/snowflake.svg           # optional, path relative to manifest
  description: |
    Scan Snowflake account permissions, table inventory, and sample data
    for sensitive content classification.

spec:
  image: { ... }                         # required
  capabilities: { ... }                  # required
  credentials: { ... }                   # optional
  source: { ... }                        # optional
  scan: { ... }                          # optional
  resources: { ... }                     # optional
  runtime: { ... }                       # optional
  permissions: { ... }                   # optional but recommended
```

## `metadata`

| field | required | status | notes |
|---|---|---|---|
| `name` | ✅ | [implemented] | Globally unique slug. Pattern: `^[a-z][a-z0-9-]{1,62}[a-z0-9]$`. Becomes `source_types.name`. |
| `displayName` | ✅ | [implemented] | What humans see in the connector picker. Up to 64 chars. |
| `version` | ✅ | [implemented] | Semver (e.g. `1.2.0`, `2.0.0-rc.1`). The runtime uses this to bind to image tags. |
| `vendor` | optional | [stored, not applied] | `netwrix` for first-party, `community` for marketplace contributions, `local` for in-house, or your org name. Stored in the database; trust-tier display and filtering are not yet implemented. |
| `icon` | optional | [stored, not applied] | Path (relative to the manifest) to an SVG/PNG. Accepted and stored, but the UI picker does not yet display it. |
| `description` | optional | [stored, not applied] | Markdown blurb intended for the connector detail view. Stored in the database; no UI renders it yet. |

## `spec.image`

```yaml
image:
  repository: ghcr.io/netwrix/connectors/snowflake   # required
  digest: sha256:abc123...                            # required for signed installs
  pullPolicy: IfNotPresent                            # default
  signing:                                            # optional
    cosign:
      certificateIdentity: https://github.com/netwrix-dev/connectors/.github/workflows/release.yaml@refs/tags/v1.2.0
      certificateOidcIssuer: https://token.actions.githubusercontent.com
```

| field | status | notes |
|---|---|---|
| `repository` | [implemented] | Full image path, e.g. `ghcr.io/netwrix/connectors/snowflake`. The tag is derived from `metadata.version`; do not declare it here. |
| `digest` | [stored, not applied] | Accepted at upload and stored; pod dispatch always uses `repository:tag`, never `repository@digest`. Digest pinning is planned for Phase 3 (cosign). |
| `pullPolicy` | [stored, not applied] | Accepted and stored; the cluster always uses `IfNotPresent` regardless of the declared value. |
| `signing.cosign` | [planned] | Schema reserved for signature verification. No verification pipeline exists yet. |

The framework launches your container as `repository:<metadata.version>`. **`digest` and `signing` are stored but not yet enforced** — see status tags above.

### Image tag = `metadata.version`

The image tag is **derived** from `metadata.version`. There is no `spec.image.tag` field; declaring one makes the schema reject the manifest. This guarantees the `source_types` row, the bundle's `image.tar`, and the running pod can never disagree about which build is running.

Why this matters: if a connector ever shipped two versions under a floating tag (`:dev`, `:latest`), kubelet's `pullPolicy: Never` lookup would silently resolve to whichever blob was imported last — not the image the new version was built from. The "new" version's bug fixes would run as the old version's bytes, with no error from kubelet at any layer. Real-world incident on the dev cluster: `dropbox-0.2.1` shipped with `v0.2.0`'s compiled bytes for a full scan cycle before anyone noticed.

**Recommended Makefile pattern** — derive both the image tag and the bundle filename from `metadata.version`:

```makefile
VERSION := $(shell grep '^  version:' connector.yaml | head -1 | awk '{print $$2}')
IMG     := localhost/connector-framework/snowflake:$(VERSION)

image:
	sudo docker build -t $(IMG) .

bundle: image
	sudo docker save $(IMG) -o image.tar
	tar -czf snowflake-$(VERSION).tar.gz connector.yaml image.tar README.md
	rm -f image.tar
```

`aa26-connector lint` rule **R007** flags any `tag:` line still present under `spec.image:` so old manifests get a clear "remove the line" hint before the schema rejects them.

## `spec.capabilities`

Tells the framework what your connector can do. The UI uses this to decide what buttons to show; the runtime uses it to validate that what you're trying to do matches what you said you'd do.

```yaml
capabilities:
  scanTypes: [access_scan, sensitive_data_scan]      # which scans this supports
  operations: [test_connection, discover, scan, fetch, apply]
  sidecars: [extraction]                             # framework utility sidecars (v1: extraction)
  additionalProcesses:
    - key: enrich_owners
      displayName: Enrich Owners
      description: Resolve principal IDs to full names via the directory.
```

**Operations** (verbs your container dispatches at runtime, picked from the invocation):

| operation | status | when called |
|---|---|---|
| `test_connection` | [implemented] | User clicks "Test connection" on Service Account or Source |
| `scan` | [implemented] | The actual work — `scan.scanType` says `access_scan` vs `sensitive_data_scan` |
| `discover` | [stored, not applied] | Stored in capabilities; no UI button or dispatch path exists yet |
| `fetch` | [stored, not applied] | Stored in capabilities; no UI button or dispatch path exists yet |
| `apply` | [stored, not applied] | Stored in capabilities; no UI button or dispatch path exists yet |

**Scan types** must be a subset of `[access_scan, sensitive_data_scan, sync]`. If you list `access_scan`, you're promising your `scan` op handles `scanType=access_scan` invocations.

**Sidecars** are framework-managed utility containers attached to your Pod. Opt in to share heavy tooling (Tika, Tesseract, OCR language packs) without bundling it into your image. v1 supports one value:

| sidecar | what it gives you | when to add |
|---|---|---|
| `extraction` | Tika + Tesseract behind one HTTP API at `127.0.0.1:8087`. Worker reads `EXTRACTION_URL` from env and POSTs file bytes to `/v1/extract`, gets text back. | Connectors that need to extract text from PDF/DOCX/XLSX/HTML/etc. — typical for SDS scans on file shares, web crawls, S3, SharePoint. |

When you opt into a sidecar, the framework attaches it to every scan Pod, sets the right env var on your worker, and runs its own probes. You write SDK calls; the framework handles tool selection, JVM tuning, OCR language packs, and the failure modes. See [extraction.md](./extraction.md) for the full integration guide.

**Additional processes** are post-processing handlers callable from a running scan via `POST /v1/process`. Skip this section unless you need it.

## `spec.credentials`

Defines the form for creating a Service Account against this connector. **JSON Schema**, with a few extensions:

```yaml
credentials:
  schema:
    type: object
    required: [clientId, clientSecret]
    properties:
      clientId:
        type: string
        x-display: "Client ID"
        description: "OAuth M2M client ID from your Snowflake account."
      clientSecret:
        type: string
        x-display: "Client Secret"
        x-secret: true            # rendered as password input; not echoed
      role:
        type: string
        x-display: "Role"
        default: "ACCOUNTADMIN"
```

Supported `x-` extensions:

| extension | meaning |
|---|---|
| `x-display` | Field label. Defaults to the property key if absent. |
| `x-secret` | Renders as a password input. Stored as a k8s Secret value, never logged. |
| `x-section` | Group fields into named sections in the form. |
| `x-conditional` | `{ field: "type", equals: "oauth" }` — only show this field when another has a given value. |

If your connector takes no credentials (e.g. it scans local files), set `properties: {}`:

```yaml
credentials:
  schema:
    type: object
    properties: {}
```

## `spec.source`

Defines the **Create Source** form — connection params for one specific instance of this connector type.

```yaml
source:
  schema:
    type: object
    required: [accountUrl, warehouse]
    properties:
      accountUrl:
        type: string
        x-display: "Account URL"
        format: uri
        x-placeholder: "https://abc-xy12345.snowflakecomputing.com"
      warehouse:
        type: string
        x-display: "Warehouse"
      catalogFilter:
        type: array
        items: { type: string }
        x-display: "Catalog filter (regex list)"
        description: "Empty = all catalogs."
```

Same `x-` extensions as `credentials`. Any valid JSON Schema is allowed — `enum` for dropdowns, `type: array` for lists, conditional fields, the works.

## `spec.scan` [stored, not applied]

Per-execution overrides — the form that would be shown when a user clicks **Run scan now** with custom parameters. The schema is stored in the database but the "Run scan now" UI does not yet surface these fields. Declare them for forward-compatibility; they will be wired once the UI gains a per-execution parameter panel.

```yaml
scan:
  schema:
    type: object
    properties:
      maxFileBytes:
        type: integer
        x-display: "Max file size (bytes) for SDS content download"
        default: 1048576
        minimum: 1024
        maximum: 52428800
      recipes:
        type: array
        items:
          type: string
        x-display: "Evidence AI recipes to run (empty = all)"
        description: |
          Restrict Evidence AI to specific sensitivity recipes for this scan.
          Leave empty (or omit) to run all enabled recipes — that is the
          default. See the recipe reference table below for available values.
      detectors:
        type: array
        items:
          type: string
        x-display: "Evidence AI detectors to run (empty = all)"
        description: |
          Reserved — accepted and stored but not yet applied by Evidence AI.
          Leave empty. The recipe filter (above) is the current knob for
          controlling what Evidence AI reports on a scan.
```

### How `recipes` and `detectors` reach Evidence AI

When the runtime sidecar starts a sensitive-data scan it fires a one-time `POST /api/v1/runtime/subscribe` to the Evidence AI API. The `recipes` and `detectors` values are included in that body, keyed by `scan_execution_id`. Evidence AI caches them and applies the recipe filter to every `/classify` call it receives for that execution. The operator never touches env vars or per-file payloads — these two fields in the scan schema are the only knob.

| Field | Type | Meaning |
|---|---|---|
| `recipes` | `string[]` | Run only these Evidence AI recipes. `[]` or absent = all recipes. |
| `detectors` | `string[]` | Reserved for future per-detector gating. Pass `[]` or omit. |

### Available recipes

Refer to the Evidence AI documentation for a list of available recipes.

## `spec.resources` [stored, not applied]

Standard k8s resources block. Accepted at upload and stored, but the cluster does not yet apply it — the connector-api uses deployment-level resource defaults for all connector pods regardless of what is declared here. Declare them for forward-compatibility.

```yaml
resources:
  requests: { cpu: "200m", memory: "256Mi" }
  limits:   { cpu: "2",    memory: "2Gi"   }
```

## `spec.extraction`

Configuration for the extraction sidecar. Only meaningful when `capabilities.sidecars` includes `extraction`. Connectors that don't use the sidecar may omit this block entirely.

```yaml
extraction:
  maxDepth: 2     # how many levels deep to recurse into archives
```

| Field | Type | Default | Range | Meaning |
| --- | --- | --- | --- | --- |
| `maxDepth` | integer | 2 | 1–10 | How deep the sidecar recurses into archives. `1` = container only (no archive unwrap). `2` = unwrap one level (typical: ZIP containing PDFs). `3+` = unwrap nested archives (ZIP-of-ZIPs). |

The framework injects this value as `EXTRACTION_MAX_DEPTH` on the sidecar. Entries returned by `/v1/extract` are filtered to drop anything past the configured depth. See [extraction.md](./extraction.md#archive-handling) for behavior details.

## `spec.runtime`

```yaml
runtime:
  type: container                 # only "container" supported in v1
  timeoutSeconds: 14400            # default 4 hours
  network:
    egress:
      - "snowflakecomputing.com"
      - "*.snowflakecomputing.com"
```

| field | status | notes |
|---|---|---|
| `type` | [stored, not applied] | Only `container` is meaningful; stored but no branching logic reads it. |
| `timeoutSeconds` | [stored, not applied] | Stored; the cluster applies a deployment-level job TTL, not the connector's declared timeout. |
| `network.egress` | [stored, not applied] | Stored; no NetworkPolicy is generated from this list yet. Wildcards are valid syntax for when generation is implemented. |

## `spec.auth`

Declares which authentication methods this connector accepts. Drives the **Credentials** section in the Add Source wizard. Connectors that don't need auth (web crawlers, public APIs) can omit this section entirely; the wizard hides the credentials panel when zero usable methods are declared.

```yaml
auth:
  methods:
    - type: none
      displayName: "Anonymous (no credentials)"
    - type: bearer
      displayName: "Bearer token"
      description: "Use a Personal Access Token from User Settings → Developer."
      fields:
        type: object
        required: [token]
        properties:
          token:
            type: string
            x-display: "Token"
            x-secret: true
    - type: basic
      displayName: "Username + password"
      fields:
        type: object
        required: [username, password]
        properties:
          username: { type: string, x-display: "Username" }
          password: { type: string, x-display: "Password", x-secret: true }
    - type: service_account
      displayName: "Existing service account"
      accountTypes: [username_password, client_id_secret]
  scope: per-source
```

### Method types

| `type` | status | Behavior |
|---|---|---|
| `none` | [implemented] | Connector takes no credentials. Wizard skips the Credentials section entirely if this is the only method declared. |
| `bearer` | [implemented] | Inline token. The wizard renders a token field; the value is stored in a per-source k8s Secret and injected into the pod. |
| `basic` | [implemented] | Username + password. Stored as separate k8s Secrets and injected into the pod. |
| `api_key` | [implemented] | API key. Stored as a k8s Secret and injected into the pod. |
| `oauth2` | [implemented] | The framework's OAuth2 engine. Handles redirect, callback, storage, refresh, and runtime token delivery. See **[oauth2.md](oauth2.md)**. |
| `service_account` | [stub] | Backend wired — existing SAs can be linked. The wizard UI currently shows an informational banner ("coming soon") in place of an SA picker. Full picker planned for v2. |
| `custom` | [implemented] | Same as inline but for connector-specific shapes that don't fit the named types above. Fields are arbitrary. |
| `customAuthAdapter` | [planned] | Escape hatch for providers that cannot be modeled declaratively. Not implemented. |

### OAuth2 fields

When `type: oauth2`, these additional fields apply (full details in **[oauth2.md](oauth2.md)**):

| Field | Required | status | Description |
|---|---|---|---|
| `provider` | yes | [implemented] | Short name (e.g. `dropbox`, `google`). Maps to deployment env vars `<PROVIDER>_OAUTH_CLIENT_ID/_SECRET` and to the callback URL path. |
| `authorizationUrl` | yes\* | [implemented] | Provider's authorize endpoint. May contain `{field}` placeholders. |
| `tokenUrl` | yes\* | [implemented] | Provider's token endpoint. May contain `{field}` placeholders. |
| `scopes` | yes\* | [implemented] | Array of scope strings requested at consent. |
| `pkce` | no | [implemented] | `true` (default) / `false`. Use `true` unless the provider genuinely doesn't support PKCE. |
| `revocationUrl` | no | [stored, not applied] | RFC 7009 revoke endpoint. Stored; not yet called on source deletion. |
| `extraAuthParams` | no | [implemented] | Provider-specific query params appended to the authorize URL (e.g. `token_access_type: offline` for Dropbox). |
| `extraTokenFields` | no | [implemented] | Non-standard token-response fields to persist + inject. |
| `urlParams` | no | [implemented] | Pre-auth user-input substitutions for tenant-scoped URLs (M365 `tenantId`). |
| `customAuthAdapter` | no | [planned] | Escape hatch for providers that cannot be modeled declaratively. Not implemented. |

\* Required unless `customAuthAdapter` is set.

### `scope`

| Value | status | Meaning |
|---|---|---|
| `per-source` (default) | [implemented] | Each Source in the group gets its own credentials. Right for SaaS apps with per-instance tokens. |
| `per-group` | [stored, not applied] | Stored in the database; the wizard does not yet offer a single shared credential prompt for the group. The value is available for when the UI is wired. |

### Cluster policy

Operators can restrict which methods the wizard offers via the `allowed_connector_auth_methods` AppSetting (comma-separated list). The wizard filters its dropdown to that list, and the backend independently rejects POSTs that try to use a disallowed type.

## `spec.sourceTypes`

Declares the ingestion mapping for each kind of finding your connector emits — which ClickHouse table to write to and how to project finding fields onto that table's columns.

```yaml
sourceTypes:
  - name: DropboxFile            # matches finding.sourceType
    domain: Artifact             # required for entities target; omit for permissions
    ingestion:
      target: entities           # "entities" (default) or "permissions"
      mapping:
        name:           $.object.name
        sourceSystemId: $.object.id
        sizeBytes:      $.object.size
        modifiedDate:   $.object.server_modified

  - name: DropboxPermission      # distinct name — must not share with DropboxFile
    ingestion:
      target: permissions
      mapping:
        permissionGrantId: $.subject.id
        aceType:           $.permissions.aceType
        memberRole:        $.permissions.memberRole
        readAllowed:       $.permissions.readAllowed
        writeAllowed:      $.permissions.writeAllowed
        deleteAllowed:     $.permissions.deleteAllowed
        manageAllowed:     $.permissions.manageAllowed
        adminAllowed:      $.permissions.adminAllowed
        listAllowed:       $.permissions.listAllowed
```

### `ingestion.target`

| Value | ClickHouse table | Finding type | Notes |
|---|---|---|---|
| `entities` (default) | `access_analyzer.entities` | `object_metadata` | File/table/object inventory. Requires `domain`. |
| `permissions` | `access_analyzer.permissions` | `access_grant` | Permission grants. No `domain` needed. `targetEntityId` and `principalId` are derived by the runtime from `object.id` and `subject.canonicalEmail` — do not map them. |

### `ingestion.mapping` — entities columns

See `aa26-connector schema entities` for the full allow-list. Key columns:

| Column | Notes |
|---|---|
| `name` | Display name in the UI. |
| `sourceSystemId` | **Recommended.** Natural key from the source (file ID, URL, table FQN). Used to derive a stable `entityId` across re-scans. Omitting it causes a warn. |
| `sizeBytes` | Object size in bytes. |
| `modifiedDate` | ISO 8601 timestamp. |
| `contentHash` | Content hash for change detection. |
| `parentId` | For hierarchical entities (domain=Artifact only). |

### `ingestion.mapping` — permissions columns

| Column | Required | Notes |
|---|---|---|
| `permissionGrantId` | recommended | Stable ID for this (principal, object, permission) tuple. Omitting causes dedup instability (warn). |
| `aceType` | recommended | `Allow` or `Deny`. |
| `memberRole` | recommended | `Owner`, `Member`, or `Guest`. |
| `readAllowed` | optional | Boolean. |
| `writeAllowed` | optional | Boolean. |
| `deleteAllowed` | optional | Boolean. |
| `manageAllowed` | optional | Boolean. |
| `adminAllowed` | optional | Boolean. |
| `listAllowed` | optional | Boolean. |

Runtime-injected columns (do not map these — the runtime derives them automatically):

- `targetEntityId` — UUIDv5 of the object, derived from `object.id`
- `principalId` — UUIDv5 of the principal, derived from `subject.canonicalEmail`
- `connectorReference` — the source ID
- `crawlTimestampUtc` — scan timestamp

### sourceType naming rules

- Each sourceType `name` must be globally unique within the manifest.
- Names must be `PascalCase` and match the `sourceType` field on the finding (e.g. finding `"sourceType": "DropboxPermission"` must have a manifest entry named `DropboxPermission`).
- **Never share a sourceType name between `object_metadata` and `access_grant` findings** — the runtime uses the name to look up the target table and column mapping; sharing it routes both finding types to a single table and drops or mis-stores one of them.
- `aa26-connector lint` rule **R008** warns when `access_scan` is declared in capabilities but no sourceType has `ingestion.target: permissions`.

## `spec.permissions`

What your connector is allowed to emit. The sidecar enforces this at runtime. `[implemented]`

```yaml
permissions:
  findingTypes:
    - access_grant
    - object_metadata
    - sensitive_match
    - "custom:snowflake.share_grant"   # custom: prefix for connector-specific
```

If you POST a finding with a `type` not in this list, the runtime sidecar drops it and logs the reason. The declared list is injected as `ALLOWED_FINDING_TYPES` at pod launch. See **[finding schema](finding-schema.md)** for the built-in types.

## Validation

Run the validator early and often:

```bash
aa26-connector validate ./connector.yaml
```

The registry validates the same way at admit time. Errors are surfaced via `kubectl -n connector-prototype get configmap connector-registry-status -o yaml` (Phase 1) or the registry's `/status` endpoint:

```bash
kubectl -n connector-prototype port-forward svc/connector-registry 8090:8090 &
curl -s http://localhost:8090/status | jq '.connectors[] | select(.state != "Ready")'
```
