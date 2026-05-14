# Manifest reference — `connector.yaml`

The manifest is the contract. Every connector ships one. The framework parses it, validates it against [the JSON Schema](https://connectors.netwrix.io/schema/connector.schema.json), and uses the result to drive the UI form, image discovery, capability gating, and (eventually) signature verification.

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

| field | required | notes |
|---|---|---|
| `name` | ✅ | Globally unique slug. Pattern: `^[a-z][a-z0-9-]{1,62}[a-z0-9]$`. Becomes `source_types.name`. |
| `displayName` | ✅ | What humans see in the connector picker. Up to 64 chars. |
| `version` | ✅ | Semver (e.g. `1.2.0`, `2.0.0-rc.1`). The runtime uses this to bind to image tags. |
| `vendor` | optional | `netwrix` for first-party, `community` for marketplace contributions, `local` for in-house, or your org name. Drives the trust tier. Default: `local`. |
| `icon` | optional | Path (relative to the manifest) to an SVG/PNG. Shown in the picker. |
| `description` | optional | Markdown blurb shown when an admin clicks into the connector. |

## `spec.image`

```yaml
image:
  repository: ghcr.io/netwrix/connectors/snowflake   # required
  tag: 1.2.0                                          # required — must match metadata.version
  digest: sha256:abc123...                            # required for signed installs
  pullPolicy: IfNotPresent                            # default
  signing:                                            # optional
    cosign:
      certificateIdentity: https://github.com/netwrix-dev/connectors/.github/workflows/release.yaml@refs/tags/v1.2.0
      certificateOidcIssuer: https://token.actions.githubusercontent.com
```

The framework launches your container as `repository@digest` if `digest` is set, or `repository:tag` otherwise. **In production, set both digest and signing**; the framework rejects unsigned community connectors unless the cluster operator explicitly allows them.

### Versioned image tags (required at package time)

**Pin `spec.image.tag` to the same value as `metadata.version`.** `aa26-connector package` enforces this strictly; `aa26-connector lint` rule **R007** warns when the tag is floating (`dev` or `latest`). The bug class this prevents:

1. Connector author releases v0.2.0; bundle ships an image tagged `:dev`. Cluster imports it, kubelet runs it under that tag — fine.
2. Author makes changes, releases v0.2.1; bundle ships a new image also tagged `:dev`. Cluster registers a new `source_types` row for v0.2.1 — but if anything in the import chain misses (image not retagged, ctr cache not refreshed, etc.), kubelet's `pullPolicy: Never` lookup for `:dev` still resolves to the v0.2.0 image bytes.
3. Pods spawned for v0.2.1 sources run v0.2.0 code. No error from kubelet (the tag resolves successfully), no warning anywhere. The "new" version's bug fixes are invisible.

Pinning the tag to `metadata.version` makes this impossible: kubelet either finds the exact image for the version row it's launching, or fails loud with `ErrImageNeverPull`. The `source_types` row, the bundle's `image.tar`, and the running pod are forced into lockstep on every release.

**Recommended pattern:**

```yaml
metadata:
  name: snowflake
  version: 1.2.0

spec:
  image:
    repository: ghcr.io/netwrix/connectors/snowflake
    tag: 1.2.0          # ← same as metadata.version above
    pullPolicy: Never   # for in-cluster registry use
```

**Mechanical:** drive both fields from a single source of truth in your release pipeline. The Makefile pattern below derives `tag` from `metadata.version` so they can't drift:

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

`aa26-connector package` rejects a manifest where `tag` and `version` disagree before running `docker save` — see [cli.md](cli.md#aa26-connector-package) for the exact failure message and remediation.

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

| operation | when called | required? |
|---|---|---|
| `test_connection` | User clicks "Test connection" on Service Account or Source | conventionally always present |
| `discover` | Discovery scan to enumerate available data | optional |
| `scan` | The actual work — `scan.scanType` says `access_scan` vs `sensitive_data_scan` vs `sync` | required if any `scanTypes` listed |
| `fetch` | UI drilldown wants the contents of one object | optional |
| `apply` | Write back, e.g. apply a sensitivity label | optional |

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

## `spec.scan`

Per-execution overrides — the form shown when a user clicks **Run scan now** with custom parameters. Skip if your scans don't need user-tunable knobs.

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

| Recipe | Detects | Severity |
|---|---|---|
| `active_credential` | API keys, secrets, private keys | Critical |
| `payment_data` | Credit cards, bank accounts, IBAN, SWIFT | Critical / High |
| `regulated_identifier` | SSN, passport, driver's licence, national IDs | High |
| `employee_record` | HR files — PII + salary / payroll | High |
| `internal_directory` | Staff lists with name + email + phone | Medium |
| `contact_list` | Customer / prospect contact lists | Medium |
| `personal_contact_info` | Standalone PII (email, phone, DOB) | Medium / Low |
| `compensation_data` | Salary data, compensation bands | High |
| `health_information` | PHI — medical codes, clinical notes | Critical / High |
| `legal_contract` | Contracts, NDAs, legal agreements (LLM) | High |

!!! note "`legal_contract` requires an LLM client"
    Without an LLM configured on the Evidence AI side this recipe is silently
    skipped regardless of `recipe_filter`.

## `spec.resources`

Standard k8s resources block applied to your container.

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

`network.egress` is advisory — the framework can derive a NetworkPolicy from it to harden the connector pod's egress. Wildcards allowed.

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

| `type` | Behavior |
|---|---|
| `none` | Connector takes no credentials. Wizard skips the Credentials section entirely if this is the only method declared. |
| `basic` / `bearer` / `api_key` | Inline credentials. The wizard renders the method's `fields` JSON Schema; values land in a per-source k8s Secret created by `ConnectorAuthHandler`. |
| `service_account` | Reuses AA26's existing Service Account picker. `accountTypes` (optional) restricts the picker to specific SA flavors. |
| `oauth2` | The framework's OAuth2 engine. Declares the provider's authorize/token URLs, scopes, and optional knobs; the framework handles redirect, callback, storage, refresh, and runtime token delivery. Your connector reads the current token from `GET /v1/credentials`. See **[oauth2.md](oauth2.md)** for the full guide. |
| `custom` | Same as inline but for connector-specific shapes that don't fit basic/bearer/api_key. Fields are arbitrary; what gets stored is opaque to the framework. |

### OAuth2 fields

When `type: oauth2`, these additional fields apply (full details in **[oauth2.md](oauth2.md)**):

| Field | Required | Description |
|---|---|---|
| `provider` | yes | Short name (e.g. `dropbox`, `google`, `microsoft`, `salesforce`). Maps to deployment env vars `<PROVIDER>_OAUTH_CLIENT_ID/_SECRET` and to the callback URL path. |
| `authorizationUrl` | yes\* | Provider's authorize endpoint. May contain `{field}` placeholders. |
| `tokenUrl` | yes\* | Provider's token endpoint. May contain `{field}` placeholders. |
| `scopes` | yes\* | Array of scope strings requested at consent. |
| `pkce` | no | `true` (default) / `false`. Use `true` unless the provider genuinely doesn't support PKCE. |
| `revocationUrl` | no | RFC 7009 revoke endpoint, called best-effort on source delete. |
| `extraAuthParams` | no | Map of provider-specific query params appended to the authorize URL (e.g. `token_access_type: offline` for Dropbox). |
| `extraTokenFields` | no | Array of non-standard token-response fields to persist + inject (e.g. `instance_url` for Salesforce). |
| `urlParams` | no | Pre-auth user-input substitutions for tenant-scoped URLs (M365 `tenantId`). |
| `customAuthAdapter` | no | Escape hatch: `host:port` your connector image exposes to handle authorize/exchange/refresh for providers the declarative engine can't model. Mutually exclusive with `authorizationUrl`/`tokenUrl`/`scopes`. |

\* Required unless `customAuthAdapter` is set.

### `scope`

| Value | Meaning |
|---|---|
| `per-source` (default) | Each Source in the group gets its own credentials. Right for SaaS apps with per-instance tokens (Databricks PAT, Box token). |
| `per-group` | One credential bundle covers every Source in the group. Right for AD bind credentials, file-server CIFS credentials. |

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

What your connector is allowed to emit. The sidecar enforces this at admission time.

```yaml
permissions:
  findingTypes:
    - access_grant
    - object_metadata
    - sensitive_match
    - "custom:snowflake.share_grant"   # custom: prefix for connector-specific
```

If you POST a finding with a `type` that isn't in this list, the sidecar rejects it. This keeps the Scan Executions tab uniform without preventing extension. See **[finding schema](finding-schema.md)** for the built-in three.

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
