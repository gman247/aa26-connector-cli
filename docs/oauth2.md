# OAuth2 authentication

When your connector talks to a SaaS data source that uses OAuth2 (Dropbox,
Google Drive, M365, Salesforce, Slack, Box, GitHub, …) you do **not** write
any OAuth code. You declare the provider in `connector.yaml` and the
framework handles redirect, callback, token exchange, storage, refresh,
and runtime token delivery. Your connector just reads the current access
token from the sidecar.

> Spec: this chapter is the connector-author guide. The full architectural
> spec lives in the dspm repo at
> `connector-framework/docs/oauth2-v1.md`. Read that if you need the
> *why* behind any of the *what* here.

## TL;DR

```yaml
# connector.yaml
auth:
  methods:
    - type: oauth2
      provider: dropbox
      displayName: "Sign in with Dropbox"
      authorizationUrl: "https://www.dropbox.com/oauth2/authorize"
      tokenUrl: "https://api.dropboxapi.com/oauth2/token"
      revocationUrl: "https://api.dropboxapi.com/2/auth/token/revoke"
      scopes:
        - files.metadata.read
        - files.content.read
      pkce: true
      extraAuthParams:
        token_access_type: offline
```

```python
# Your connector — three lines
import requests
creds = requests.get("http://127.0.0.1:8089/v1/credentials").json()
client = dropbox.Dropbox(oauth2_access_token=creds["access_token"])
# ...scan...
```

That's it. The framework owns everything between "user clicks Connect" and
"the access token shows up in your container."

## The split of responsibilities

| Who | Owns |
|---|---|
| **Framework** | The "Connect with X" button. Browser redirect. OAuth callback. Token exchange. Encrypted storage. Proactive refresh. Refresh-token rotation. Runtime token vending. Re-authorization UX when refresh fails. Revocation on source delete. |
| **You (the connector author)** | Pick the provider's URLs and scopes. Call `GET /v1/credentials` to get a token. Retry on 401. That's the whole job. |
| **The customer (deployment operator)** | Register an OAuth app at the provider (one bullet in the install runbook). Drop the resulting `client_id` / `client_secret` into the AA deployment's env vars. |

There are **no provider-specific code paths in the framework**. The engine is
configuration-driven from your manifest. Adding a new provider means writing
a manifest entry, not a code change to AA26.

## When to use OAuth2

Pick `type: oauth2` when the data source authenticates users via an OAuth2
authorization-code flow:

- Dropbox, Google Drive, Google Workspace
- Microsoft 365 / SharePoint / OneDrive (delegated access)
- Salesforce
- Box, Slack, Notion, Asana, GitHub, GitLab
- Most modern SaaS APIs

Stick with the other `auth.methods[]` types when:

- The data source uses a static API token the user pastes into a form → `bearer` or `api_key`.
- The data source uses a service-account JSON file → `service_account`.
- There's no auth (public web crawl) → `none`.
- The data source uses OAuth2 *client credentials* (server-to-server, no
  user) — this is **not yet supported** by the framework's OAuth engine.
  Track the spec for when it lands.

## The manifest block

Full reference. Every field except the four required ones is optional.

```yaml
auth:
  methods:
    - type: oauth2

      # === REQUIRED ===

      provider: dropbox
      # Short name. Maps to deployment env vars
      # <PROVIDER>_OAUTH_CLIENT_ID / <PROVIDER>_OAUTH_CLIENT_SECRET
      # and to the callback URL path /api/v1/oauth/callback/<provider>.
      # Match across all releases of your connector so the customer's
      # provider app keeps working through upgrades.

      displayName: "Sign in with Dropbox"
      # Label on the "Connect" button in the source-create UI.

      authorizationUrl: "https://www.dropbox.com/oauth2/authorize"
      tokenUrl:         "https://api.dropboxapi.com/oauth2/token"
      # The provider's OAuth endpoints. May contain {field} placeholders
      # that get substituted from `urlParams` (see M365 example).

      scopes:
        - files.metadata.read
        - sharing.read
        - files.content.read
      # What you ask the user to grant. The framework requests these at
      # consent time. The provider may return fewer (the user can shrink
      # the consent); the framework persists what was actually granted
      # and compares against this list at scan time. Drift triggers
      # re-authorization.

      # === STRONGLY RECOMMENDED ===

      pkce: true
      # PKCE (RFC 7636). Default true. The framework generates the
      # code_verifier + code_challenge automatically. Only set to false
      # for providers that genuinely don't support PKCE — none of the
      # major ones in 2026.

      revocationUrl: "https://api.dropboxapi.com/2/auth/token/revoke"
      # RFC 7009 token revocation endpoint. Framework POSTs here when
      # the user deletes the source so the provider can tear down its
      # side of the grant. Best-effort — source delete proceeds even if
      # revocation fails.

      iconUrl: "/assets/providers/dropbox.svg"
      # Optional. Framework ships defaults for known providers.

      # === PROVIDER-SPECIFIC OPTIONS ===

      extraAuthParams:
        token_access_type: offline
      # Provider-specific query params appended to the authorize URL.
      #   - Dropbox needs token_access_type=offline to get a refresh token.
      #   - Google needs access_type=offline (and usually prompt=consent).
      #   - Some providers use this for response_mode, request_uri, etc.

      urlParams:
        - field: tenantId
          x-display: "Tenant ID"
          description: "Your Azure AD tenant ID or domain"
      # Pre-auth user inputs substituted into authorizationUrl/tokenUrl
      # templates. The framework renders a form for these before the
      # redirect. M365 needs this for tenant-scoped URLs.

      extraTokenFields:
        - instance_url
      # Token-response fields beyond RFC 6749 §5.1 standard that the
      # framework should persist and inject at runtime. Salesforce
      # returns instance_url; you read it back via creds["extras"]
      # at runtime to know which API base URL to hit.

      # === ESCAPE HATCH (rare) ===

      customAuthAdapter: "127.0.0.1:9090"
      # When the provider's flow genuinely can't be modeled by the fields
      # above. The framework calls /oauth/* endpoints on this host:port
      # inside your container. See "The escape hatch" below.
      # Mutually exclusive with authorizationUrl/tokenUrl/scopes etc.
```

## The runtime contract

Once the user has authorized, your connector reads the current token from
the sidecar:

```
GET 127.0.0.1:8089/v1/credentials
```

Response:

```json
{
  "access_token": "sl.B1234567890abcdef...",
  "token_type":   "Bearer",
  "expires_at":   "2026-05-11T18:42:13Z",
  "scope":        "files.metadata.read files.content.read",
  "extras": {
    "instance_url": "https://na1.salesforce.com"
  }
}
```

`access_token` is what you put in your `Authorization: Bearer …` header (or
hand to your provider SDK). `extras` carries any `extraTokenFields` values
your manifest declared.

### When to call it

**The 401-retry path is the correctness contract.** Whenever your provider
returns 401 (Unauthorized), assume the token expired and call
`GET /v1/credentials` again. The sidecar will transparently refresh through
core-api and return the fresh token. Pseudocode:

```python
def call_provider(method, url, **kw):
    creds = get_creds()
    resp = requests.request(method, url,
                            headers={"Authorization": f"Bearer {creds['access_token']}"}, **kw)
    if resp.status_code == 401:
        creds = get_creds(force_refresh=True)  # see below
        resp = requests.request(method, url,
                                headers={"Authorization": f"Bearer {creds['access_token']}"}, **kw)
    return resp

def get_creds(force_refresh=False):
    # Sidecar handles the refresh decision itself based on expires_at.
    # Just call again — if cache is fresh, returns instantly.
    r = requests.get("http://127.0.0.1:8089/v1/credentials", timeout=5)
    r.raise_for_status()
    return r.json()
```

**The proactive-refresh path is optimization.** You MAY call
`/v1/credentials` between batches (e.g. every 100 files) as a hint to refresh
ahead of expiry. The sidecar's cache makes this cheap; the refresh only
fires when needed. This eliminates the latency hit of the 401-retry path on
the *next* request after expiry.

### Handling permanent refresh failure

If the refresh token itself dies (user changed password, admin revoked the
app, ~90-day idle expiry on M365), the sidecar returns **503** with header
`X-OAuth-State: needs_reauthorization`:

```
HTTP/1.1 503 Service Unavailable
X-OAuth-State: needs_reauthorization
Content-Type: application/json

{"error":"needs_reauthorization","detail":"refresh token expired"}
```

When you see this, fail the scan with a clear error code so the operator
notices:

```python
if resp.status_code == 503 and resp.headers.get("X-OAuth-State") == "needs_reauthorization":
    log_error("OAUTH_REAUTHORIZATION_REQUIRED",
              "Source needs to be re-connected at /lab/evidence")
    sys.exit(0)  # let the sidecar /v1/complete the scan cleanly
```

The framework will already have transitioned the source to
`needs_reauthorization` state and the webapp will show a "Re-connect" button
on the source detail page. You don't have to handle the UX.

### Detecting non-OAuth scans

`GET /v1/credentials` returns **404** when the source is not configured for
OAuth (e.g. the source uses `auth: bearer` instead). If your connector
supports multiple auth methods, check for 404 and fall through to the
appropriate alternative path. If your connector is OAuth-only, treat 404 as
a misconfiguration error.

## Provider examples

### Dropbox (simple)

```yaml
auth:
  methods:
    - type: oauth2
      provider: dropbox
      displayName: "Sign in with Dropbox"
      authorizationUrl: "https://www.dropbox.com/oauth2/authorize"
      tokenUrl:         "https://api.dropboxapi.com/oauth2/token"
      revocationUrl:    "https://api.dropboxapi.com/2/auth/token/revoke"
      scopes:
        - account_info.read
        - files.metadata.read
        - sharing.read
        - files.content.read
      pkce: true
      extraAuthParams:
        token_access_type: offline
```

### Google Drive

```yaml
auth:
  methods:
    - type: oauth2
      provider: google
      displayName: "Sign in with Google"
      authorizationUrl: "https://accounts.google.com/o/oauth2/v2/auth"
      tokenUrl:         "https://oauth2.googleapis.com/token"
      revocationUrl:    "https://oauth2.googleapis.com/revoke"
      scopes:
        - https://www.googleapis.com/auth/drive.readonly
      pkce: true
      extraAuthParams:
        access_type: offline
        prompt: consent
```

Note `prompt: consent` — without it Google only issues a refresh token on
the first consent, and a re-consent after revocation comes back without one.
`prompt: consent` forces re-issuance every time.

### Microsoft 365 (tenant-scoped, delegated)

```yaml
auth:
  methods:
    - type: oauth2
      provider: microsoft
      displayName: "Sign in with Microsoft"
      authorizationUrl: "https://login.microsoftonline.com/{tenantId}/oauth2/v2.0/authorize"
      tokenUrl:         "https://login.microsoftonline.com/{tenantId}/oauth2/v2.0/token"
      urlParams:
        - field: tenantId
          x-display: "Tenant ID"
          description: "Your Azure AD tenant ID or 'common'/'organizations' for multi-tenant"
      scopes:
        - https://graph.microsoft.com/Files.Read.All
        - offline_access
      pkce: true
```

`offline_access` is mandatory in M365's delegated flow — without it, no
refresh token gets issued and you have to re-consent every hour.

### Salesforce (instance_url in token response)

```yaml
auth:
  methods:
    - type: oauth2
      provider: salesforce
      displayName: "Sign in with Salesforce"
      authorizationUrl: "https://login.salesforce.com/services/oauth2/authorize"
      tokenUrl:         "https://login.salesforce.com/services/oauth2/token"
      revocationUrl:    "https://login.salesforce.com/services/oauth2/revoke"
      scopes: [api, refresh_token]
      pkce: true
      extraTokenFields:
        - instance_url
```

At runtime your connector reads `creds["extras"]["instance_url"]` and uses
it as the base URL for every Salesforce API call (it varies per
tenant — `na1.salesforce.com`, `eu5.salesforce.com`, etc.).

### Slack (bot token + user token in same response)

```yaml
auth:
  methods:
    - type: oauth2
      provider: slack
      displayName: "Install in Slack"
      authorizationUrl: "https://slack.com/oauth/v2/authorize"
      tokenUrl:         "https://slack.com/api/oauth.v2.access"
      revocationUrl:    "https://slack.com/api/auth.revoke"
      scopes:
        - channels:read
        - files:read
      pkce: false        # Slack does not yet support PKCE (as of 2026-05)
      extraTokenFields:
        - bot_user_id
```

Slack returns `access_token` (bot token) and a nested `authed_user.access_token`
(user token, if user scopes were requested). For this manifest only the bot
token is used. If you need the user token, declare an `extraTokenFields:
[user_access_token]` entry — provided the provider response actually carries
a top-level `user_access_token`. Otherwise model the dual-token case via the
escape hatch.

## Customer install runbook

When your connector ships, the customer needs to register an OAuth app at
the provider and drop two env vars onto the AA deployment. **Provide a
runbook** in your connector's README. Skeleton (substitute your provider):

```markdown
## Register a Dropbox app for your AA deployment

What you'll need:
- Admin access to https://www.dropbox.com/developers/apps
- Your AA deployment hostname (e.g. aa.example.com)
- ~10 minutes

Steps:
1. Sign in at https://www.dropbox.com/developers/apps and click "Create app".
2. Choose API: "Scoped access". Type: "Full Dropbox" or "App folder" per
   your scanning scope.
3. Name: "Access Analyzer — <your org>". This shows on the consent screen.
4. Under Permissions, grant:
   - account_info.read
   - files.metadata.read
   - sharing.read
   - files.content.read
5. Under OAuth 2, add Redirect URI:
   https://<your-aa-hostname>/api/v1/oauth/callback/dropbox
6. Copy the App key (`client_id`) and App secret (`client_secret`).
7. On the AA deployment, set env vars on core-api:
   DROPBOX_OAUTH_CLIENT_ID=<App key>
   DROPBOX_OAUTH_CLIENT_SECRET=<App secret>
8. Restart core-api.
9. In the AA webapp, create a new Dropbox source. Click "Sign in with
   Dropbox" and authorize with a Dropbox account that has access to the
   data you want to scan.

Troubleshooting:
- "Redirect URI mismatch" → URI in step 5 must match the AA callback URL
  exactly (https, no trailing slash).
- "needs_reauthorization" appears after a long idle period → expected
  when the refresh token expires or the user revokes the app. Click
  "Re-connect" on the source detail page.
```

## Local testing

> **Status:** these flags are the planned OAuth2 test-harness surface
> landing alongside the framework's OAuth2 v1 implementation.
> Token-injection mode is the highest priority and the recommended way
> to test today. Mock-provider and real-browser modes ship in a
> follow-up CLI release. See **[test-harness](test-harness.md)** for the
> current state.

Token-injection: skip the auth dance, hand the harness a real or
synthetic token directly.

```bash
aa26-connector test --auth oauth2 \
  --oauth-token 'sl.B1234567890.fake-or-real' \
  --oauth-extras 'instance_url=https://na1.salesforce.com' \
  ./my-connector
```

For end-to-end testing against a real provider (planned), register a
dev app at the provider (separate from your customer's production app)
and drop its credentials into your test environment:

```bash
export DROPBOX_OAUTH_CLIENT_ID=devapp-id
export DROPBOX_OAUTH_CLIENT_SECRET=devapp-secret
aa26-connector test --provider dropbox --browser ./my-connector
```

This opens a real browser, completes the consent at Dropbox, then drives
your connector with the resulting token.

## The escape hatch — `customAuthAdapter`

Most OAuth providers fit the declarative manifest above. When yours
genuinely doesn't — non-standard token response shape that can't be
modeled by `extraTokenFields`, a provider-specific signing step before
the redirect, a token-binding mechanism the engine doesn't understand —
you can opt out of the engine for that one method:

```yaml
auth:
  methods:
    - type: oauth2
      provider: weird_corp
      displayName: "Sign in with WeirdCorp"
      customAuthAdapter: "127.0.0.1:9090"
```

When set, the framework calls into **your connector image** at the declared
host:port over HTTP. Your container must expose three endpoints:

```
POST /oauth/authorize_url
  Body:  { state, code_challenge, url_params, scopes, redirect_uri }
  Reply: { authorize_url }

POST /oauth/exchange
  Body:  { code, code_verifier, redirect_uri, url_params }
  Reply: { access_token, refresh_token?, expires_in, scope?, extras: {...} }

POST /oauth/refresh
  Body:  { refresh_token, url_params }
  Reply: { access_token, refresh_token?, expires_in, scope?, extras: {...} }
```

The framework passes `client_id` and `client_secret` to your container via
env vars (`<PROVIDER>_OAUTH_CLIENT_ID` and `_SECRET`). Your adapter handles
them privately and never returns them to the framework.

The framework continues to own:
- The browser-redirect dance
- Persisting `state` for CSRF and refresh
- Token storage (you return the bundle; the framework stores refs)
- The `needs_reauthorization` state machine
- The `/v1/credentials` sidecar contract

The adapter is just three HTTP endpoints that decide what to put in the
authorize URL and how to interpret the token response. Three days of work,
not a fork of the engine.

**When NOT to use the escape hatch:**
- Provider returns extra fields you want to inject → use `extraTokenFields`.
- Provider needs a query param like `access_type=offline` → use
  `extraAuthParams`.
- Provider's URL is tenant-scoped → use `urlParams`.
- Provider doesn't return a `scope` field in the token response → it's
  fine, the framework falls back to the manifest scopes.

The escape hatch is for genuinely-weird providers (signed requests, mTLS
to the token endpoint, non-RFC token formats). 95% of OAuth providers
work without it.

## What the framework will NOT do

- **Application-mode OAuth** (Client Credentials grant, two-legged auth)
  is not yet supported. Track the spec for when it lands.
- **Device Authorization Grant** (CLI / TV-style flows) is not supported.
- **Token formats other than Bearer** (DPoP, MTLS-bound tokens) are not
  supported.
- **Per-call signing** (some banking APIs require signing each request
  with a key derived from the OAuth token) is your connector's job; the
  framework just delivers the access token.

## Versioning and stability

Once you ship a connector that declares `oauth2`, the manifest fields you
use are part of your stability surface:

- Removing `revocationUrl` after customers have authorized is fine — old
  tokens just don't get revoked on source delete.
- Removing a scope is fine — but old tokens still carry it, scope-drift
  detection won't fire.
- Adding a new scope to an existing `scopes:` list triggers re-authorization
  the next time the source scans. The customer's user has to click
  "Re-connect" and grant the expanded scope set. Plan this around release
  windows.
- Renaming `provider` is **a breaking change**. Customer apps are
  registered with a redirect URI keyed on provider name; renaming means
  every customer has to update their provider-side app config.

When in doubt: add new manifest fields, don't rename existing ones.

## Reference

- Spec: `dspm:connector-framework/docs/oauth2-v1.md`
- Manifest schema: `dspm:connector-framework/schema/connector.schema.json`
  (the `auth.methods[].type=oauth2` block)
- Runtime endpoint: `/v1/credentials` — see
  **[runtime-contract.md](runtime-contract.md)**
- Test harness OAuth options: **[test-harness.md](test-harness.md)**
- Example connector that uses OAuth2: **[examples/dropbox.md](examples/dropbox.md)**
