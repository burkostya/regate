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

## Management surface

- `regate_effects`: list current status projections.
- `regate_effect_decide`: explicitly resolve one `unknown` effect
  with `effect_id`, `actor`, `verdict` and `reason`.
- REST equivalent: `POST /api/effect-decide` (admin bearer required).

## Remaining work before issue #6 can close

The stored read-back interface and state machine do not themselves constitute
a set of verified adapters. Configurable read-back contracts, effect-specific
resource identity, idempotency keys shared with external systems, durable
capability grants, execution leases and robust compensation remain required.
Only property-verified adapters may claim an authoritative negative outcome.
Audit actor identity is currently a management assertion authenticated by
the shared admin bearer, not an individually signed attestation. No
production exactly-once guarantee is claimed.
