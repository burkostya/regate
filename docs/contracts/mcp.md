# MCP HTTP transport and discovery contract

Regate supports HTTP MCP only. Standard inbound public and administrative
`tools/*`, `resources/*`, `prompts/*`, `initialize`, `server/discover` and
`subscriptions/listen` use the official Go MCP SDK. Older unversioned Regate
discovery and task-start `resultType: task` are narrow compatibility bridges.

## Upstream catalog refresh

The management `regate_upstream_refresh` method returns:

- `items`: tool descriptors, retained for older management API clients;
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

## Test matrix

| Case | Automated check |
|---|---|
| Modern inbound tool/resource/prompt/public and admin skills | Independent official SDK client tests + raw HTTP conformance |
| Legacy 2025-06-18 negotiation | Official SDK legacy server fixture + raw HTTP initialize |
| Modern method/name/metadata mismatch | Raw HTTP negative tests, must fail before side effects |
| Body-size rejection | Raw HTTP payload > 4 MiB -> 413 |
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
