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
  tag: 1.2.0                                          # optional
  digest: sha256:abc123...                            # required for signed installs
  pullPolicy: IfNotPresent                            # default
  signing:                                            # optional
    cosign:
      certificateIdentity: https://github.com/netwrix-dev/connectors/.github/workflows/release.yaml@refs/tags/v1.2.0
      certificateOidcIssuer: https://token.actions.githubusercontent.com
```

The framework launches your container as `repository@digest` if `digest` is set, or `repository:tag` otherwise. **In production, set both digest and signing**; the framework rejects unsigned community connectors unless the cluster operator explicitly allows them.

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

## `spec.resources`

Standard k8s resources block applied to your container.

```yaml
resources:
  requests: { cpu: "200m", memory: "256Mi" }
  limits:   { cpu: "2",    memory: "2Gi"   }
```

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
| `custom` | Same as inline but for connector-specific shapes that don't fit basic/bearer/api_key. Fields are arbitrary; what gets stored is opaque to the framework. |

### `scope`

| Value | Meaning |
|---|---|
| `per-source` (default) | Each Source in the group gets its own credentials. Right for SaaS apps with per-instance tokens (Databricks PAT, Box token). |
| `per-group` | One credential bundle covers every Source in the group. Right for AD bind credentials, file-server CIFS credentials. |

### Cluster policy

Operators can restrict which methods the wizard offers via the `allowed_connector_auth_methods` AppSetting (comma-separated list). The wizard filters its dropdown to that list, and the backend independently rejects POSTs that try to use a disallowed type.

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
