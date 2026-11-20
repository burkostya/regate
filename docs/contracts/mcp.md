# MCP HTTP transport and discovery contract

Regate supports HTTP MCP only. Standard inbound public and administrative
`tools/*`, `resources/*`, `prompts/*`, `initialize`, `server/discover` and
`subscriptions/listen` use the official Go MCP SDK. Older unversioned Regate
discovery and task-start `resultType: task` are narrow compatibility bridges.

## Upstream catalog refresh

The management `regate_upstream_refresh` method returns:

- `items`: tool descriptors, retained for older management API clients;
- `tool_schema_fingerprints`: lowercase SHA-256 fingerprints of observable
  input and output schemas by unambiguous tool name;
- `resources`, `resourceTemplates` and `prompts`: independent upstream
  descriptors from standard MCP list methods;
- `revision`: immutable Regate configuration revision used for this discovery;
- `at` and optional `error`: time and failure diagnostics.

The upstream client opens **one** official SDK session for a complete catalog.
It collects all supported pages of all advertised collections, with a maximum
of 100 pages and 10,000 objects per collection. A repeated cursor, invalid
entry, oversized result, timeout or network failure fails closed. Resources
and prompts are requested only when the upstream advertises the relevant
capability; tools/list is also attempted for older, incompletely advertising
servers. An SDK session is always closed after the operation.

An upstream may lie about its descriptors. Discovery is **not** a security
attestation: listed descriptions, JSON Schemas and annotations are untrusted.
They are not automatically published to a Regate surface.

A cache entry is usable only for the same Regate revision; activation and
rollback invalidate the cache, and a discovery that completes after a revision
switch cannot overwrite the new cache. A cached failure is returned as an
error, never silently converted into a successful empty list.

## Tool schema pins

To require a known MCP tool contract, inspect the authenticated management
`regate_upstream_refresh` result for the upstream, review the tool's
`inputSchema` and `outputSchema`, and copy its `tool_schema_fingerprints`
value into `tool_schema_pins` under that `upstream` in an HCL draft. Validate
and activate the draft to start enforcing it. Each map key is the actual
upstream tool name, including when an exposed Regate tool has a different
name. Existing HCL without pins keeps its dispatch behavior.

Before calling a pinned tool, Regate follows bounded `tools/list` pages in
the same SDK session and under the same upstream authorization as the call.
It stops before sending arguments or admitting a durable effect when the
catalog is unavailable or the tool is missing, duplicated or mismatched.
This applies to direct tools, pipeline nodes, MCP hooks, readback and
compensation. A configured safe-read retry rechecks the pin in its own
session. Dry-runs continue to use configured mocks.

Fingerprints hash the two SDK-decoded schema JSON objects with sorted object
keys. A missing output schema is represented as `null`. Tool descriptions
and annotations outside the schemas do not affect the hash. A changed schema
requires an explicitly reviewed new pin or restoration of the pinned schema.
Management refresh only reports observations. Pins are part of the immutable
HCL revision and do not change reconciliation tokens for previously attempted
writes.

This check compares advertised schemas. It cannot make the list and call
atomic on the provider, and it cannot establish the behavior of a provider
that returns a matching descriptor. Existing runtime validation still
checks configured data contracts. Resource and prompt descriptors have no
schema pins.

## Test matrix

| Case | Automated check |
|---|---|
| Modern inbound tool/resource/prompt/public and admin skills | Independent official SDK client tests + raw HTTP conformance |
| Legacy 2025-06-18 negotiation | Official SDK legacy server fixture + raw HTTP initialize |
| Modern method/name/metadata mismatch | Raw HTTP negative tests, must fail before side effects |
| Inbound body-size rejection | Raw HTTP payload > 4 MiB -> 413 |
| Outbound request body budget | 4 MiB fully encoded HTTP body accepted; one-byte excess blocked before network/tool dispatch |
| Upstream pagination | Official SDK small pages across tools/resources/templates/prompts |
| Malicious repeated cursor / excessive items | Unit tests for bounded collector |
| Unknown result on interrupted write | Official SDK no-blind-retry test |
| Request cancellation | HTTP cancellation while policy is pending, assert no protected call |
| SSE catalog changes | SDK subscription stream notified on activation |
| Revision-scoped cache and in-flight requests | Activation tests |

## Remaining release gates

This is not a substitute for production OAuth, issuer/audience/scope checks
(issue #5), SQLite event durability, effect reconciliation and event replay
(issue #6), full task/skill cross-process interoperability, stress/fault tests
and independent external-server conformance (issue #9). Stateless HTTP does
not imply replayable SSE sessions: disconnected listeners must reconnect and
refresh their catalog until a durable event store is implemented and tested.

## Outbound MCP request boundary (#9)

Every outgoing HTTP MCP request with a body is buffered under a **4 MiB
total-body limit** in the credential transport, before DNS resolution, network
dispatch or an upstream tool side effect. The check covers the serialized
JSON-RPC envelope and any transformed arguments, not only the raw input map.
The limit is inclusive; one extra byte rejects the request without sending
a partial body. The transport does not expose `GetBody` for buffered calls:
unknown write results must never become eligible for transparent replay.
The independent 8 MiB upstream response budget remains in force.

This is a per-request limit, not a global throughput quota, a complete SSE
stream-volume policy, or a comprehensive data-classification guarantee.

## Bounded SDK subscriptions/listen lifetime and admission (#9)

The official Go MCP SDK handles 2026-07-28 `subscriptions/listen` streams.
Regate limits these **actual authenticated SDK requests** before dispatch:
at most **64 concurrent listeners per process**, with an atomic capacity
reservation. A 65th receives HTTP 503 and `Retry-After: 5` **before**
the SDK creates an SSE session or acknowledges it. Cancellation releases
the slot. A listener has a 30-minute maximum lifetime, and the SDK's outgoing
SSE writes are guarded by a 10-second write deadline.

The SDK continues to own protocol parsing, subscription filters,
acknowledgment ordering and notifications; no alternate custom SSE
implementation or competing notification bus is used. HTTP tests exercise
the actual SDK stream, overflow rejection and notification delivery after
activation.

This is local capacity protection, **not** per-principal/global fairness,
cross-process notification fanout, replayable SSE, resource update watchers
or comprehensive load/chaos acceptance.
