# MCP HTTP transport and discovery contract

Regate supports HTTP MCP only. Standard inbound public and administrative
`tools/*`, `resources/*`, `prompts/*`, `initialize`, `server/discover` and
`subscriptions/listen` use the official Go MCP SDK. Older unversioned Regate
discovery and task-start `resultType: task` are narrow compatibility bridges.

## Publish an existing MCP server

Use a surface with an `upstream` reference to publish the complete Streamable
HTTP interface. Start from [the migration example](../../examples/publish-upstream.hcl)
or add the surface to a configuration draft and activate it. Change the
client's endpoint to the surface URL, which defaults to `/mcp/<surface name>`.
Use `path` for a different endpoint within `/mcp/`. Declaring an upstream
alone keeps it available only to Regate's configured callers.

The surface requires an explicit `auth` value:

| `auth` | Regate endpoint | Upstream |
|---|---|---|
| `anonymous` | Anonymous access to this explicitly published route | No configured credentials |
| `bearer` | Checks the secret named by the surface's `bearer_token_env` | Uses the dedicated bearer named by the upstream's `auth_env` |
| `oauth` | Checks the surface's `oauth` provider, endpoint audience and `required_scopes` | [Exchanges the incoming token](oauth.md#delegated-upstream-credentials-rfc-8693) for the configured upstream audience and scopes |

For static Bearer migration, the operator can set the client-facing
environment variable to the bearer the client already uses. When moving to
a new OAuth audience, the client follows the Regate resource metadata and
can sign in again. The authorization server must support RFC 8693 exchange
to the upstream audience, and both provider configurations must use the
same issuer.

The upstream supplies discovery, descriptors, extensions, access to each
object and execution semantics. Regate forwards JSON-RPC and streams SSE
as it arrives. Headers tied to the connection or gateway credentials stay
at their HTTP boundary. Regate rejects upstream HTTP redirects and applies
the guarded egress configuration, public admission controls, 4 MiB request
limit, 8 MiB finite response limit, and 30-minute stream lifetime. The
configured `timeout_ms` bounds the wait for upstream response headers and for
a finite response body. This mode covers stateful and stateless Streamable
HTTP. The older two-endpoint HTTP+SSE transport requires a separate migration.

Review the whole upstream catalog before activating the surface. The console
and `regate_impact_analyze` show its path, upstream and authorization mode.
Curated surfaces remain the way to apply Regate's graph, per-tool policies,
schema pins, masking, interceptors and effect journal. A published upstream
surface cannot mix in local tools, resources or prompts. The proxy emits
transport diagnostics without recording MCP messages or credentials.

### Session loss and recovery

For stateful Streamable HTTP, Regate maps a client-facing session ID to the
upstream session and binds it to the authenticated owner and the endpoint
query. The per-surface cache holds up to 1,024 sessions and 32 MiB of
initialization messages, with 30-minute idle expiry. State lives in memory.
Changing the upstream destination or authorization invalidates its session
IDs. Existing requests finish on the revision they entered.

HTTP `404` on a known session and the upstream error `Invalid session ID`
trigger recovery, including plain HTTP `400` and a matching JSON-RPC error
in a finite response or in the first SSE event of a read request.
Regate initializes one replacement session for overlapping callers and
keeps the public session ID. A discovery or read request can be sent once
to the replacement session if the response has yet to start. A stateful
`tools/call` and other potentially effectful requests are checked with
`ping` before dispatch. If the session is lost after that check, Regate
returns an error without sending the operation again.

An SSE cursor is valid for the specific upstream session that issued it.
After replacing that session, a reconnect can open a fresh stream while
missed events and upstream application state remain subject to the
upstream's own durability. A Regate process restart requires the client
to initialize a new session.

## Test a curated resource or prompt

Use management `regate_test` with `surface` and exactly one of `tool`,
`resource`, or `prompt`. The resource selector is its HCL block name.
A prompt test can pass `arguments` as an object of string values. Send
`draft_id` and `expected_source_digest` to test saved draft source, or
`expected_revision` to test the active revision.

In a resource or prompt test, `mocks.source` represents an upstream
`resources/read` or `prompts/get` result. `mocks.hooks` maps interceptor
names to synthetic native hook responses, such as a Decision API response
`{ "decision": "allow" }` or an MCP tool result with `structuredContent`.
Provide fixtures for every source, decision and transform reached by the
test. External error observers run only on real calls. Builtin hooks, CEL, shape validation and response masking run locally.
Tests use operator-supplied fixtures without using a client bearer, making
outbound calls, or generating effect records. Test actual OAuth scopes and
delegation separately on the public surface.

`regate_impact_analyze` and `regate_diff` include `content_changes` with
the before and after stages, scopes and destinations of affected objects.
Activation and rollback apply the resulting configuration to new requests.

## Upstream catalog refresh

The management `regate_upstream_refresh` method returns:

- `items`: tool descriptors, retained for older management API clients;
- `tool_schema_fingerprints`: lowercase SHA-256 fingerprints of observable
  input and output schemas by unambiguous tool name;
- `resources`, `resourceTemplates` and `prompts`: independent upstream
  descriptors from standard MCP list methods;
- `revision`: immutable Regate configuration revision used for this discovery;
- `at` and optional `error`: time and failure diagnostics.

The upstream client uses one official SDK session per catalog attempt.
It collects all supported pages of all advertised collections, with a maximum
of 100 pages and 10,000 objects per collection. A repeated cursor, invalid
entry, oversized result, timeout or network failure fails closed. Resources
and prompts are requested only when the upstream advertises the relevant
capability; tools/list is also attempted for older, incompletely advertising
servers. Each SDK session is closed after use.

For upstream calls handled by the SDK, Regate validates complete HTTP JSON
responses before decoding. The 8 MiB limit applies to the full body. Duplicate
member names, invalid UTF-8, excessive nesting and trailing data fail closed
without including the response payload in diagnostics. Empty HTTP 202 and 204
responses remain valid. SSE remains streamed under its existing byte limit.

HTTP 404 on an upstream request carrying `Mcp-Session-Id` signals a lost
legacy session, regardless of whether the body is plain text or JSON-RPC.
Regate reports `upstream MCP session expired` without including the ID or
response body. A catalog refresh can open one replacement session within its
original timeout. It then reads every collection from the beginning and
discards any earlier pages and cursors.

Tool calls use the [explicit HCL read retry policy](hcl.md#operator-attested-bounded-mcp-read-retry-pr-53)
for session recovery. Once an external write has been dispatched, loss of
its response leaves an unknown outcome in the effect journal for reconciliation.

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
| Upstream session lost during pagination | Restart before page two, no old cursor or partial catalog survives |
| Session loss during tool calls | One policy-approved read recovery; dispatched write stays unknown |
| Malicious repeated cursor / excessive items | Unit tests for bounded collector |
| Unknown result on interrupted write | Official SDK no-blind-retry test |
| Request cancellation | HTTP cancellation while policy is pending, assert no protected call |
| SSE catalog changes | SDK subscription stream notified on activation |
| Regate restart | Public and admin calls at the same URL with the same SQLite state; reconnect SSE and refresh |
| Revision-scoped cache and in-flight requests | Activation tests |

## Remaining release gates

This is not a substitute for production OAuth, issuer/audience/scope checks
(issue #5), SQLite event durability, effect reconciliation and event replay
(issue #6), full task/skill cross-process interoperability, stress/fault tests
and independent external-server conformance (issue #9).

Public and administrative MCP HTTP requests are stateless. Clients can send
new requests once Regate is listening again, using the same endpoint and
credentials. After an abrupt exit, the previous SQLite execution lease can
remain active for up to 30 seconds. Starting Regate with `-wait-for-leader`
lets it wait for the lease on the same state directory.
An existing SSE connection closes when Regate stops. Clients must open a
new `subscriptions/listen` stream, wait for acknowledgment, and refresh
their catalog. Events missed during the disconnect are unavailable for replay.

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
