# Security and operational boundaries

The [v1.0.0-rc.1 prerelease](https://github.com/burkostya/regate/releases/tag/v1.0.0-rc.1)
passed local acceptance with synthetic data. The
[security and PII review](operations/threat-review.md#v100-rc1-security-and-pii-review)
lists the tested controls and the evidence still needed for sign-off.
[Issue #10](https://github.com/burkostya/regate/issues/10) tracks that
decision and independent audit custody. Use the prerelease on isolated
stands with synthetic data pending sign-off.

## Identity and management

- The CLI requires `REGATE_ADMIN_TOKEN`. Management REST and MCP require
  that bearer. Public OAuth credentials grant access only to their authorized
  public operations. The console keeps the admin token in browser memory and
  sends it through its same-origin management proxy.
- On a public surface without OAuth, `REGATE_PUBLIC_TOKEN` is required
  when configured. Anonymous access is limited to loopback peers when the
  token is unset. The CLI requires a public token for non-loopback listeners.
- An OAuth surface verifies issuer, audience, subject, expiry and scopes
  using JWT signatures with JWKS or opaque-token introspection. Tool,
  resource and prompt scopes govern discovery and invocation. Task access
  also checks the originating surface, the original scopes, and the owner
  bound to the verified issuer, audience and subject. See the
  [OAuth resource-server contract](contracts/oauth.md).
- Regate keeps inbound bearer credentials at the resource-server boundary.
  Outbound credentials come from environment variables named by `auth_env`
  or a separately verified RFC 8693 exchange with an explicit target
  audience. Keep secret values out of HCL, draft inputs, UI layout, URLs and
  diagnostics.
- Activation validates HCL and rejects stale draft bases. Use
  `expected_source_digest` to pin the reviewed draft content. In-flight runs
  keep their revision.

## Decision and data boundaries

- OAuth execution preflight treats data as Restricted and requires explicit
  Restricted trust declarations for external destinations in the pipeline
  and its interceptors. Trust declarations and caller scopes are separate
  checks. Outside loopback, the outbound HTTP guard requires HTTPS hosts
  listed in `REGATE_ALLOWED_HOSTS`. It applies IP checks at dial time and
  blocks redirects and environment HTTP proxies.
- Decision denial, indeterminate results, invalid responses and unsupported
  obligations stop the protected path before its effects. In the acceptance
  pipeline, the agent receives scrubbed data. A scrubber failure stops the
  agent call and the downstream write.
- OAuth tool responses and task results mask recognized PII and credentials
  by default, including nested fields. An unmasked tool must explicitly
  declare Restricted data, opt in with `allow_unmasked`, and require
  `pii:read`. OAuth resource and prompt text also passes through the mask.
  Opaque OAuth resource blobs are rejected. The mask uses recognizable
  patterns and field names. A reviewer must assess data formats and external
  redactors for the intended use.
- The demonstration tool `get_customer_direct` on the legacy demo
  surface deliberately returns the fixture's unmasked data. This direct
  proxying example belongs on an isolated, synthetic-data stand.

## Durable state and diagnostics

- SQLite holds configuration revisions, tasks, suspension checkpoints,
  effects, traces and audit records. Queued inputs and provisioned OAuth task
  grants use separate encryption keyrings. HCL revisions and ordinary
  suspension checkpoints can contain plaintext. Protect the state directory,
  SQLite WAL and backups as sensitive files. See the
  [SQLite contract](contracts/sqlite.md).
- The effect journal preserves an uncertain write outcome for reconciliation.
  Verified readback can resolve it using evidence bound to its original
  intent. An absent record under a positive-only readback contract leaves
  the operation uncertain.
- The administrative audit stores a fixed event schema and a chained
  digest. Pipeline traces and effect diagnostics retain bounded metadata and
  sanitize recognized sensitive values. They are separate from the audit
  schema and may contain configured names. Metrics and optional OTLP/HTTP
  transport spans use fixed labels.
  See [telemetry and audit](operations/telemetry.md).
