# HTTP admission, telemetry and privacy boundaries

Operational gateway metrics are available from the **admin-only** `GET /api/metrics` endpoint. It returns a bounded set of method, route-class, status-bucket, request count and aggregate duration counters, plus current admission occupancy and OTLP drops. The route classes are constants (`public_mcp`, `admin_mcp`, `admin_api`, `oauth_metadata`, `health`, `index`, `not_found`); the caller's actual path, query, header, token, remote address, MCP tool name, input and output never become labels. Never expose /api/metrics without the admin bearer.

Regate reserves **128 concurrent public** and **16 concurrent management** HTTP requests per process. An exhausted pool rejects requests with HTTP 503 and `Retry-After: 1` *before* MCP parsing or external effects. A common 4 MiB inbound request-body cap also applies to chunked transfer encoding. Streamable HTTP and SSE preserve HTTP flushing/hijack support. These are deliberately coarse process-local capacity barriers, **not** per-tenant quotas, fair scheduling, distributed rate limiting or a replacement for a load balancer.

Set `REGATE_OTLP_TRACES_URL` to an **approved**, absolute OTLP/HTTP JSON `/v1/traces` endpoint to enable trace export. Loopback HTTP is allowed for local testing; external HTTPS requires the collector hostname in `REGATE_ALLOWED_HOSTS`. The existing DNS-rebinding, proxy and redirect restrictions also apply. A bounded 256-span queue, 32-span batches and 2-second send deadline ensure a failing collector never blocks MCP requests; `otlp_spans_dropped` counts queue overflow and export failures. Events contain only fixed-route HTTP transport metadata, random IDs and timestamps. No request body, token, per-user identity, raw path or remote data is exported. The collector receives no upstream credentials. Export is disabled by default.

For a shared quota on authenticated public MCP requests, set
`REGATE_OAUTH_QUOTA_KEY` to a separate secret of 32 random bytes encoded
as hex. Configure `REGATE_OAUTH_QUOTA_RPM` (default 600) and
`REGATE_OAUTH_QUOTA_BURST` (default 60) within 1–10000. All Regate
processes using the same state directory must use identical settings
and share a host clock. A key change starts new quotas. Deliver the key
through the deployment's secret store.

Admission is charged after the gateway verifies the OAuth token and
surface scopes. A verified issuer and subject share one budget across
surfaces, tokens, reverse proxies and same-host process failover. Exhaustion
returns HTTP 429 and `Retry-After` before tool dispatch, and SQLite failure
returns HTTP 503. Process-local socket-peer and in-flight limits continue
to apply. Management and public surfaces without OAuth retain those limits.
The shared quota has no per-client metrics labels and covers requests to
open streams rather than stream duration. See the
[durability contract](../contracts/sqlite.md#shared-oauth-request-quota-schema-v7).

Use the admin-only effect history to correlate durable transitions with a
pipeline run and node. Read a specific effect via `regate_effect_history`
or `/api/effect-history?effect_id=ID`, then filter
`regate_pipeline_trace` or `/api/traces?run_id=ID` on the returned run ID.
The journal retains transitions across restart. OTLP exports transport spans
from a bounded queue and may drop telemetry during a collector outage.

## Durable administrative audit (SQLite v6)

Regate now records `started` and terminal `succeeded` / `failed` entries for management operations that change configuration, invoke effects, resume work, rewrap keys or reconcile effects. REST layout mutations are covered too. The admission event must commit before the administrative operation starts. Failed admissions deny the action. A missing terminal record after a crash or audit failure means **inspect durable state before retry** (especially for effectful operations), never infer that the operation did not run.

`GET /api/audit` and the `regate_audit` management MCP tool expose only authenticated, bounded recent records. Records contain fixed category/action/outcome, sequence, UTC time and a chained SHA-256 digest; they never include HCL source, inputs, results, bearer tokens, addresses, identity strings, error messages or approval reasons. SQLite startup validates the full chain. Deletion, mutation and sequence gaps fail closed; unit tests cover restart, concurrent append and corruption.

The sequence and chain head in a verified backup can be anchored off-host.
An independently stored checkpoint lets operators detect a rewritten history
or a truncated snapshot before restore, including rewrites with a valid local
hash chain. Production use requires an independently held signing key, a
retention policy, and tested access to the off-host archive. Authentication
denials remain aggregate HTTP metrics to limit unauthenticated disk use.

### Off-host audit checkpoints

On a trusted verifier, stream the JSON manifest from
`regate -verify-backup /secure/backups/regate.sqlite`. The manifest contains
`audit_events` (the sequence number), `audit_head_sha256` and the SHA-256
of the standalone snapshot. Send the manifest to the independent audit
custodian. The custodian signs the exact file with its Ed25519 key and
archives the file and signature in an off-host, append-only inventory tied
to the deployment. A subsequent snapshot must extend the latest accepted
checkpoint before the custodian signs it. Use the last independently
accepted sequence after a recovery event too. Checkpoint frequency sets the
maximum interval of history still awaiting independent attestation.

```sh
openssl pkeyutl -sign -inkey /secure/regate-audit-private.pem \
  -rawin -in checkpoint.json -out checkpoint.sig
```

An auditor can reproduce the verification with OpenSSL and jq. The enrolled
public key comes from the custodian's trusted inventory.

```sh
openssl pkeyutl -verify -pubin -inkey /secure/regate-audit-public.pem \
  -rawin -in checkpoint.json -sigfile checkpoint.sig &&
regate -verify-backup /secure/backups/candidate.sqlite \
  -audit-anchor-sequence "$(jq -er '.audit_events' checkpoint.json)" \
  -audit-anchor-sha256 "$(jq -er '.audit_head_sha256' checkpoint.json)"
```

For exact restoration of the checkpointed snapshot, also compare the
candidate's `sha256` in the returned manifest with the signed file.
For operational acceptance, attach evidence of independent key custody, the
chosen retention settings and an exercised restore to
[#113](https://github.com/burkostya/regate/issues/113).
