# Effect lifecycle and reconciliation contract

An effectful MCP call must be registered in the SQLite event journal **before**
the remote request is transmitted. The write-admission barrier belongs to
SQLite; an in-process mutex alone cannot prevent two workers from submitting
the same logical attempt.

## Stable identities

An external write is identified by a SHA-256 digest of a domain separator and
the length-delimited triple `(run_id, node_id, upstream.operation)`. The
canonical JSON input is hashed separately as `intent_digest` and never
persisted in plaintext inside the effect journal.

- First `BeginEffect` inserts `dispatching` in one SQLite transaction.
- Another worker with the same run/node/operation cannot dispatch, whether
  the first attempt remains `dispatching`, was `confirmed`, became
  `unknown`, or has already been explicitly resolved.
- A different argument digest for the same logical attempt is a conflict,
  not an opportunity to reissue the write with a new request.
- After a confirmed response, `CompleteEffect` records `confirmed`.
  A lost response records `unknown`; startup also maps interrupted
  `dispatching` to `unknown`. Neither case ever retries automatically.
- A missing/failed journal write stops before dispatch; failure to persist a
  completion is never reported as confirmed success.

A new **intentional** execution has a different run identity. This barrier
does not deduplicate completely independent client requests lacking a shared
idempotency key. Shared run groups and external idempotency keys require
explicit contracts; do not claim exactly-once external execution from this
milestone alone.

## HTTP POST and hook identity

HTTP `http.call` nodes and HTTP/Decision hooks are treated as **writes**:
a POST is not automatically read-only merely because its response is used as a
transform or a policy decision. A SHA-256 hash of the endpoint URL and the
canonical request body identifies the intent without persisting the raw URL,
credentials or arguments. Before calling the HTTP endpoint, Regate requires an
active SQLite execution lease and atomically journals `dispatching`.

A fully parsed HTTP 200 JSON object completes the effect as `confirmed`.
An HTTP failure, lost connection, malformed response or leader loss after the
barrier completes as `unknown`, never as safe-to-retry. A known application
decision such as `deny` is recorded as a completed *external request* and
then enforced locally; confirmation does not imply authorization.

Hooks now use the parent run identity and a stable hierarchical step key
(`run / node / interceptor stage and name`). This prevents an accidentally
fresh random ID from bypassing the write barrier when the same logical step is
retried during a resume. Independent client requests still have distinct run
identities; remote idempotency keys remain a separate contract.

## Reconciliation

`ResolveWithReadback` accepts a trusted in-process `ReadbackVerifier`
implementation that actually queries the relevant external resource. There
is no Jira-specific assumption: the adapter may use MCP, HTTP or a verified
resource-specific API. It must return the same `effect_id`, `intent_digest`
and `operation`, plus a concrete resource, a method, a verifier identifier,
a digest of the read response and a verdict.

A positive authoritative match can resolve as `resolved_applied`. A negative
read-back may resolve as `resolved_absent` **only if the adapter explicitly
attests authoritative absence**, rather than relying on an eventually
consistent not-found response. Unverified or contradictory observations leave
the effect `unknown`. A competing resolution is refused.

`ResolveByOperator` is a separate mechanism. A management administrator
supplies a named actor, `applied` / `absent` verdict and explanation; the
journal marks the resolution kind `operator`, not `readback`. This can be
used when the external service lacks a reliably verifiable read-after-write
contract. Operator text is masked before persistence; access to Management
MCP and the equivalent REST endpoint remains admin-only.

The resolution event is appended; earlier event versions remain immutable.
A resolved attempt still cannot be automatically redispatched. Starting a
new run after an operator decision is a separate intentional action.

## Resource-bound MCP positive read-back

A top-level HCL `readback` declaration configures a trusted read-only MCP
lookup to reconcile an `unknown` write. Each newly dispatched MCP effect
carries a `target_digest` identifying the actual upstream URL (SHA-256,
without persisting the raw URL). The readback adapter may run only if the
stored target and `upstream.tool` operation match the configured target.

The administrator supplies the **original client write arguments**. Their
canonical SHA-256 must match the effect's stored `intent_digest` before any
external read is made. When a per-effect write token is configured, Regate
reconstructs the transmitted arguments by injecting that reserved token from
the stored effect ID before computing this digest. The caller must not supply
or override the token. One JSON Pointer extracts a stable identity from these arguments;
the declared read tool is called with that identity, and a second pointer
extracts the returned identity. By default an exact identity match is
required; a readback contract may also declare the **paired**
`match_intent_pointer` and `match_result_pointer` for an additional
exact semantic payload match. Missing, malformed or mismatched values
refuse resolution. All declared comparisons must pass before returning
`resolved_applied` with a digest of observed MCP evidence.
Missing results, a different identity, unsupported payloads, tool errors or
eventually consistent not-found results all retain `unknown`: no
`resolved_absent` inference is made.

### Optional provider-persisted operation identity

An HCL `readback` can set **both** `write_token_argument` and
`result_token_pointer`. On any matching MCP write, Regate computes the
existing stable journal effect ID before admission, rejects a caller-provided
reserved field, injects this ID as the named top-level write argument, and
journals the exact enriched request digest. Its configured provider tool
must accept this extra argument. On reconciliation, the read-only tool
must return the **same operation ID** at the configured JSON Pointer,
in addition to identity and any optional payload matches. The
readback will refuse identical-ID, identical-payload historical records
carrying a different operation ID, and will keep the effect `unknown`.

This is only valid for a **verified provider** that atomically persists the
operation ID with the write, cannot let a different actor forge an applied
receipt, enforces the required uniqueness/immutability and exposes
sufficiently consistent reads. The fixture can demonstrate those properties
under its controlled code. An arbitrary MCP provider, a user-created token,
a merely matching string or the Go SDK itself cannot establish such
attestation. No automatic retry, unconfirmed absence, general idempotency
or external fencing guarantee follows from this optional token mapping.

`safe_read = true` is an **operator assertion** about the declared tool's
behavior, not a protocol-generated proof. Only a resource-specific adapter
with verified identity/consistency semantics should be trusted in production.
A malicious MCP server that invents a matching identity can defeat this
positive-only evidence check. The current management call uses the active HCL
revision; changing the endpoint blocks reconciliation of old effects rather
than silently checking a new server. Revision-pinned adapter attestation is
outstanding work.

## Operator-approved compensating effects

Compensation is a **new external action**, not a rollback of SQLite, and must
not be interpreted as proof that the original resource state was restored.
An HCL `compensation` declaration binds the original `upstream.tool` to
an inverse MCP tool on the **same endpoint**; `enabled=true` asserts the
operator has reviewed the inverse. The authenticated management caller must
separately supply the original `effect_id`, the exact original write
arguments, a named contract, an actor and a nonempty approval explanation.

Regate refuses compensation unless the source effect is `resolved_applied`,
its original request digest matches the supplied arguments, and the original
target fingerprint matches the inverse endpoint. `confirmed` is insufficient:
receipt of an MCP result alone does not prove an application-level commit.
An `unknown`, `resolved_absent` or legacy-unbound source is also refused.

SQLite atomically journals the inverse as `dispatching` **before** its
outbound request. The inverse effect has an immutable
`compensates = <original ID>` link and an operator approval record containing
the actor and only SHA-256 of the free-text reason. Its identifier is fixed
by the original effect ID, independent of the chosen inverse contract:
a second inverse attempt using a different adapter is still blocked.
Concurrent callers admit exactly one inverse. A lost response remains
`unknown` and is never automatically retried.

A successful MCP result records the **compensation request** as `confirmed`,
not as proof of restoration; a remote error is `unknown` because the external
effect may have committed. The original effect remains unchanged. Verification
that the inverse achieved its target state, idempotency/fencing of external
operations, provider-specific compensation semantics and independent actor
attestation remain release gates.

## Management surface

- `regate_effects`: list current status projections.
- `regate_effect_compensate`: explicitly authorize a one-time inverse through
  `effect_id`, `contract`, original write `intent`, `actor` and `reason`.
- REST equivalent: `POST /api/effect-compensate` (admin bearer required).
- `regate_effect_readback`: reconcile a target-bound unknown MCP effect with
  `effect_id`, a named HCL `adapter`, and original write `intent` (object).
- REST equivalent: `POST /api/effect-readback` (admin bearer required).
- `regate_effect_decide`: explicitly resolve one `unknown` effect
  with `effect_id`, `actor`, `verdict` and `reason`.
- REST equivalent: `POST /api/effect-decide` (admin bearer required).

## Remaining work before issue #6 can close

The positive-only readback adapter verifies an identity match under an
operator-attested read-only contract; it does not verify vendor-specific
consistency and cannot prove absence. Provider-verified readback semantics,
independently enforced idempotency/fencing, durable scoped capability grants
and provider-verified compensation remain required.
Only property-verified adapters may claim an authoritative negative outcome.
Audit actor identity is currently a management assertion authenticated by
the shared admin bearer, not an individually signed attestation. No
production exactly-once guarantee is claimed.

### Evidence messages are not raw payload vaults

The SQLite event writer now redacts and bounds `Effect.Reason`
and operator `EffectResolution.Reason` before persistence.
The immutable effect identity and the cryptographic evidence/intent
digests remain unchanged. Investigations must retain protected
original evidence in the authorized source system, not assume an
arbitrary upstream error string is safe or complete proof.
This redactor does not attest a provider's state or authorize
reconciliation by itself.
