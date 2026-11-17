# OAuth resource-server contract

Regate does **not** issue tokens. Every protected MCP surface is an OAuth
resource server. A trusted external authorization server issues access tokens;
Regate verifies them before exposing a catalog, tool, resource, prompt or task.

```hcl
oauth "company" {
  issuer     = "https://idp.example.com/"
  audience   = "https://regate.example.com/mcp/support"
  jwks_url   = "https://idp.example.com/.well-known/jwks.json"

  # Optional RFC 7662 path for opaque access tokens:
  # introspection_url = "https://idp.example.com/oauth2/introspect"
  # client_id_env     = "COMPANY_INTROSPECTION_CLIENT_ID"
  # client_secret_env = "COMPANY_INTROSPECTION_CLIENT_SECRET"
}

surface "support" {
  path            = "/mcp/support"
  oauth           = "company"
  required_scopes = ["regate:access"]

  tool "customer_read" {
    source             = "crm.get_customer"
    description        = "Scoped CRM customer lookup"
    required_scopes    = ["crm:read"]
    data_classification = "restricted"
    read_only          = true
  }
}
```

The actual tool/upstream/pipeline names and references must also exist in the
same HCL file. The example is an authorization fragment.

## Fail-closed verification

- Exactly one of the existing OAuth providers must be named by a protected
  surface. Anonymous loopback and `REGATE_PUBLIC_TOKEN` do **not** bypass it.
- Signed JWT validation supports pinned JWKS keys for RS256, PS256 and ES256,
  rejects `alg=none`, unsupported key types, duplicate claims and wrong
  signatures. RSA keys must have at least 2048 bits. The JOSE `kid` must match
  one verified key and every JWT must have an exact issuer, resource audience,
  subject and unexpired `exp`; `iat`/`nbf` are enforced when present.
- Opaque tokens can be verified through an explicitly configured RFC 7662
  introspection endpoint. The `active` bit is required and the same issuer,
  audience, subject and expiry checks apply. Introspection credentials are
  loaded from **environment variable names**; they are never stored in HCL.
- All JWKS/introspection HTTP requests use the guarded outbound client, deny
  redirects and apply DNS/IP egress checks. External HTTPS hostnames must be
  explicitly included in `REGATE_ALLOWED_HOSTS`. Loopback is for testing.
- Per-surface scopes are required for every request; per-tool, per-resource and
  per-prompt scopes are also enforced on calls and filtered from discovery.
  Authentication does not imply unrestricted catalog enumeration.
- The management REST/MCP endpoints retain their separate
  `REGATE_ADMIN_TOKEN` requirement. That token does not authenticate to
  OAuth-protected public surfaces, and OAuth access tokens never authorize
  management operations.
- Resource discovery is published at
  `/.well-known/oauth-protected-resource` plus the MCP surface path, with
  RFC 9728 `resource`, `authorization_servers` and `scopes_supported`.
  Rejected OAuth requests provide an appropriate Bearer challenge.
- Inbound access tokens are **not** forwarded to upstream MCP or HTTP
  endpoints; outbound credentials come only from explicitly configured
  `auth_env` values. This prevents wrong-audience token pass-through.

## Sensitive results and async tasks

With OAuth enabled, tool content and structured results are recursively
PII/credential-masked by default. A tool may opt into an unmasked release
**only** with all three declarations: `data_classification="restricted"`,
`allow_unmasked=true`, and `required_scopes` containing `pii:read`.
Such an explicit release remains an operator-controlled declassification
decision; it is not a claim that the destination is trustworthy.

Resource and prompt text is masked by default. Opaque upstream binary
resources are denied when the gateway cannot establish how to redact them.
Diagnostic errors are generic for OAuth-protected invocations, and traces
never persist raw OAuth operation error strings. Raw tokens and credentials
must never be included in log fields or configuration values.

Async tasks retain their creation-time tool scopes and masking obligation.
After token renewal, task access is bound to the verified issuer, audience and
subject rather than the exact bearer bytes. Calls to get, update and cancel
re-check both the original tool scopes and the originating MCP surface.
The unfinished durable token-exchange/continuation contract remains separate.

## What is not proven by this milestone

The current PII classifier is not a formal proof of full declassification:
names, arbitrary identifiers, multimedia and other sensitive content can still
require dedicated DLP policies/verified transforms. End-to-end taint tracking,
delegated OAuth token exchange with target-audience validation, revocation
propagation, live validation against each external IdP, strong audit
attestations and production threat review remain release gates. Do not use
these tests to claim general-purpose safe handling of arbitrary personal
information or a complete v1 release.

## Delegated upstream credentials (RFC 8693)

The incoming bearer has the **Regate** resource audience, and must not be
forwarded to another MCP or HTTP service. Configure an explicit target
audience, least-privilege target scopes, exchange endpoint and a protected
client secret reference:

```hcl
oauth "company" {
  issuer                     = "https://idp.example.com/"
  audience                   = "https://regate.example.com/mcp/support"
  jwks_url                   = "https://idp.example.com/.well-known/jwks.json"
  token_endpoint             = "https://idp.example.com/oauth2/token"
  exchange_client_id_env     = "REGATE_EXCHANGE_CLIENT_ID"
  exchange_client_secret_env = "REGATE_EXCHANGE_CLIENT_SECRET"
}

upstream "crm" {
  url            = "https://crm.example.com/mcp"
  timeout_ms     = 10000
  trust_level    = "restricted"
  oauth_exchange = "company"
  oauth_audience = "https://crm.example.com"
  oauth_scopes   = ["crm:read"]
}
```

The exchange uses `urn:ietf:params:oauth:grant-type:token-exchange`,
`subject_token_type=access_token`, the explicit target audience and scopes.
Only a new bearer credential is used upstream. Its signature (or trusted
introspection), issuer, **target** audience, owner subject and scopes are
independently validated before dispatch. The exchanged credential is held only
in the operation's in-memory HTTP client; it is not put in the MCP connection
pool, revision store, task checkpoint, effect journal or trace. Neither static
`auth_env` and exchange nor two competing issuer configurations may be used
for one outbound operation. HTTP hooks can request exchange with the same
`oauth_exchange`, `oauth_audience`, `oauth_scopes` fields.

### Restricted data trust boundary

Every input arriving on an OAuth-protected surface is conservatively treated
as `restricted` at an external effect boundary. The full pipeline graph and
its interceptors are inspected **before any effect**. MCP upstreams and
external hooks must explicitly declare `trust_level="restricted"`, including
policy hooks and agent MCP tools; a direct `http.call` node must also declare
`trust_level="restricted"`. An omitted trust level is **public** and cannot
receive restricted data. A privileged operator's explicit trust designation
does not bypass the caller's independent scopes or PII response obligations.
The built-in non-network PII mask is local and does not need a trusted
external adapter.

This is an intentionally conservative composition rule. It does **not**
infer arbitrary property-level taint, certify external redactors or prove
remote providers actually uphold declared trust. Real attestation and
declassification evidence are separate v1 work.

### Async exchange: live execution and reauthenticated continuation

A delegated asynchronous task may now execute while its creator's **verified
subject token remains live in process memory**. The initial MCP task-start
request must authenticate the caller, carry the original required surface and
tool scopes, and provide a bearer with sufficient lifetime. Regate runs the
task in a detached context with the bearer bound to the original token's
`exp` (with a safety margin) and the configured execution deadline. Each
RFC 8693 outbound exchange rechecks token expiry and its target audience.
An expired token cannot authorize further external effects.

SQLite task state stores only an owner identity digest, creation-time scopes,
original revision and an immutable `delegation` metadata record containing
the OAuth provider name, issuer and **Regate** audience. Bearer/refresh tokens,
exchanged credentials and OAuth client secrets are **never** persisted there.

After a pipeline suspends for user input, `tasks/update` must carry a newly
verified caller bearer. Its issuer, audience and subject must match the
persisted task owner and provider, and it must still satisfy the **union**
of original surface and tool scopes. Regate resumes using the original
immutable HCL revision and then performs a fresh target-audience exchange
for each remote operation. The context-free legacy `TaskUpdate` entry point
cannot resume delegated work. Cancelled, failed and already completed tasks
cannot be resumed through this mechanism.

This is **not an autonomous durable delegated grant**. If Regate crashes
during an in-process delegated task, the task fails closed under the normal
effect reconciliation rules; the subject bearer is not recoverable. A
suspended task may persist its non-secret owner binding, but progress requires
an interactive new authorization. Full unattended post-restart execution
requires a separately designed encrypted, revocable grant store with audited
renewal, fencing, expiry, capability restrictions and recovery semantics.
This milestone does not implement those guarantees.

### Sealed task-grant vault (schema v3, inactive authorization)

The SQLite store has an **internal-only** encrypted, per-task refresh credential
vault. AES-256-GCM keys come exclusively from `REGATE_GRANT_KEYS_JSON` and
are never part of HCL. Associated authenticated data binds task, issuer,
audience, provider, revision, scopes digest, expiry and generation. Grants can
be explicitly revoked and rewrapped onto a new active key.

A refresh request is treated as a potentially non-idempotent remote effect:
before contacting a provider, the vault durably marks the grant `refreshing`.
If a response is lost or the process stops, it becomes `needs_reauth`;
the old credential is **not** blindly used again. A successfully verified
new credential must be written with compare-and-swap before effects resume.

No external refresh-token admission endpoint or autonomous resume is enabled
by this milestone. In particular, the existence of a sealed credential must
**never** be treated as evidence that an OAuth issuer authorized its use.
Interactive continuation (fresh caller bearer) is still the only enabled
delegated execution mode. Key rotation is manual and actual secure erasure
of historical encrypted records and backups requires operational controls.


### Verified task-grant provisioning via public MCP (PR #28)

A suspended, OAuth-protected task can now acquire a task-scoped, sealed
credential through the **task owner's protected MCP endpoint**, not the
separate management bearer:

- `tasks/grants/provision` accepts `taskId` and the **explicitly obtained**
  `refreshToken` as a short-lived request argument. It verifies the current
  caller, original surface+tool scopes, immutable task revision, configured
  provider/issuer/audience and the task owner. The task must be
  `input_required` and must actually need delegated OAuth.
- **Before making a refresh-token request** to the IdP, SQLite reserves a
  one-shot, task-bound `refreshing` row. Even if a remote response is lost,
  a second provisioning attempt will not repeat the external request.
- The configured OAuth endpoint is called using the explicit refresh-token
  grant and the client's credential references from environment variables.
  The returned access token must verify against the original Regate resource
  issuer, audience, exact owner identity, expiration and the **full union**
  of task scopes. A bounded, refreshed token and its nonzero
  `refresh_token_expires_in` are required; only the rotated refresh
  credential is AES-GCM sealed after verification.
- A failed or ambiguous response persists `needs_reauth`. The task's
  original grant cannot be silently recreated or used to retry. Revocation
  is available through `tasks/grants/revoke`, and non-secret state through
  `tasks/grants/status`. Management MCP does not register these methods.
- All errors are generic at the public MCP boundary. A grant credential
  is never returned to the client, published as a tool, or included in logs,
  HCL, task records, effect journals, task results or ordinary traces.

The provisioned grant is **not yet used to automatically resume tasks**.
There is no unattended scheduler in this milestone: merely having a sealed,
verified refresh credential is not an independent authority to retry a
failed task, repeat an unknown effect or ignore cancellation and revocation.
The next acceptance stage must enforce grant state again at each actual
effect boundary, atomically rotate the refresh credential before execution
and refuse unsafe crash recovery.

### Explicit grant-backed delegated continuation (PR #29)

Regate now exposes an **admin-authorized, explicit** continuation of an
already-suspended delegated task, using a previously provisioned sealed
refresh credential: `regate_task_resume_grant` or
`POST /api/task-resume-grant`. This is not an unattended scheduler or a
general mechanism to restart failed work. The task must remain
`input_required`, with its original durable checkpoint and active grant;
an expired/unknown/revoked grant or a failed or completed task is rejected.

Admission atomically reserves the checkpoint's task (`working`,
`refreshing_grant`) and grant (`refreshing`) **before** contacting the OAuth
provider. Two concurrent resumptions cannot consume the same checkpoint.
After a fresh, verified refresh response, the rotated credential is
encrypted and committed with generation-CAS before a new access token is
used for target-audience RFC 8693 exchange. The new bearer exists in memory
only and is bound by its expiry and the task execution deadline.

Every delegated MCP/HTTP effect admission checks that the original task
is still active and that its exact grant generation remains active.
Cancellation or explicit grant revocation fences subsequent effects; an
already in-flight remote request cannot be revoked without the upstream's
own idempotency/fencing guarantees. Unknown external writes are still
never retried. If the refresh response is missing/invalid, or the process
crashes during rotation, the task fails closed, the grant becomes
`needs_reauth`, and the old checkpoint is retained for inspection.

The administrator does not receive the refreshed token or impersonate a
user: the server validates the owner from the IdP's signed or introspected
resource-audience access token, and the effective authorization remains
bound to the task's original issuer, audience, subject, scopes and HCL
revision. This **explicit** continuation is not automatic cross-process
recovery. Automatic durable scheduling, stronger revocation guarantees,
secure credential key lifecycle and real-provider verification remain
release gates.

## Strict OAuth provider JSON framing (#5 / #9)

JWT protected-header and claim documents, RFC 7662 introspection claims,
JWKS key sets and token-exchange responses are checked as complete JSON
frames, including nested objects. Duplicate member names (also those
written with Unicode escapes), invalid UTF-8, trailing documents and
nesting beyond 128 levels fail closed, before any authorization verdict
or delegated bearer can be accepted. The existing 4 KiB JWT-header,
64 KiB JWT-claims/introspection/exchange and 1 MiB JWKS byte bounds
remain unchanged; refresh grant responses reuse the claims decoder.

Provider errors and sensitive tokens are not reflected back to callers.
These syntax guarantees do not establish external IdP trust, revocation
propagation, policy attestation, or authorization-code/PKCE handling.
