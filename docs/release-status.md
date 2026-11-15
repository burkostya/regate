# Regate release acceptance / implementation status

**Target scope (user-approved):** full first release includes HTTP-only MCP client/server, editable virtual tools/resources/prompts, HCL2 declarative config, typed transformations/CEL, DAG branches and compositions, HTTP/MCP hooks including agents/PII/Decision API, policies, revisioned management MCP and skills, SvelteKit+Svelte Flow visual editor, async tasks, diagnostics, security and tests. **Do not silently split or drop these features.**

**Current deliverable:** a Go engineering prototype with HCL2/CEL typed IR and JSON Schema port contracts (PRs #11 and #12), official-SDK upstream HTTP client (#13) and an incoming SDK migration (#15), plus SvelteKit frontend source. NOT complete v1. This distinction is important when planning installation or deployment.

## Implemented and locally verified

| Area | Status | Evidence |
|---|---|---|
| Go CLI + local HTTP server | Implemented | `go build`, smoke test |
| Upstream HTTP MCP client via official Go SDK v1.8.0 | Partial implementation: HTTP only; per-operation sessions and SDK protocol negotiation; no production OAuth | Official SDK modern/legacy test servers and integration tests |
| Public tools with direct source and DAG pipeline | Implemented subset | `TestEndToEndMCP`, `TestMCPDirectProxyPreservesContent` |
| HTTP Decision API / PII transform / MCP agent hook | Implemented subset | `TestEndToEndMCP`, deny test |
| `on_request`, `before_call`, `after_call`, `before_response`, `on_error` hooks | Partial implementation | Go code and integration paths |
| HCL2 parsing/cty, CEL, closed block schemas and typed IR | Implemented subset in PR #11; Go GitHub CI passed | `internal/config`, `internal/expr` and negative/source-preservation tests |
| DAG conditional edges/parallel waves/join | Basic implementation | Code path; incomplete formal/race coverage |
| Prompt/resource publishing and proxy resource read | Partial implementation | Server method path |
| Management MCP and REST endpoints | Implemented subset, official SDK inbound implementation (#15; conformance pending) | Auth + draft/activate/conflict/rollback, SDK client interoperability and skills tests |
| Immutable revision history + drafts | File-backed implementation | Integration tests |
| Dry-run with explicit mocks | Partial implementation | REST API path |
| Effect journal (`dispatching`, `confirmed`, `unknown`) | Partial implementation | Go executor |
| Async MCP tasks, cancellation, checkpoint and resume | Partial implementation | Task integration test |
| Subscription change notification | Implemented subset | SSE activation test |
| Outbound HTTP DNS/IP safety guard | Implemented initial fail-closed guard | `internal/netguard` unit tests (DNS rebinding / mixed DNS / loopback) |
| Admin skills + `skills/list`, `skills/get` / `skill://` resources | Implemented subset | Admin skill test |
| SvelteKit + Svelte Flow editor | **Type-check and build verified** | GitHub Actions npm install/check/build passed; browser E2E remains open |
| Go race tests, vet, build | **Passed** | `go test -race ./...`, `go vet ./...`, `go build ...` |

## Unfinished functionality required for full v1

1. Finish HCL2/CEL/typed-IR acceptance (#3): cross-port JSON Schema typing, runtime validation and opaque-draft source-safe patches exist, but arbitrary JSON Schema subsumption and browser-level editor round trips remain future work.
2. Verify end-to-end protocol conformance and extensions against real servers and clients. Incoming public/admin standard MCP requests use the official SDK in PR #15; only versionless `server/discover` and task-start `resultType: task` remain narrow compatibility bridges. OAuth, long-lived upstream subscriptions, cross-process task/skill interoperability and robust upstream/session reuse remain open.
3. Full JSON Schema input/output validation, compatibility checks and source-map/opaque-safe bidirectional editing of arbitrary HCL2 fragments.
4. Full composition semantics (`pipeline.call`, map, controlled retries, named ports, safe joins with effects), robust task and MRTR resume, and property-based execution tests.
5. Production-grade effect reconciliation/idempotency/compensation, backpressure and durable scheduling (no blind retries after uncertain writes).
6. Enforced data labels/taint tracking and verified PII obligations, permissions, principal identities and trust boundaries throughout every node.
7. Persist revisions/events/traces/tasks in SQLite with transactions; safe migrations and recovery/compaction.
8. OAuth and scoped permissions for admin, user/surface, and upstreams; production-audited egress/SSRF rules with explicit internal-service configuration; secure secret handling.
9. Independent MCP resource/prompt discovery (including templates, pagination and revision-aware caching) now exists; source-safe mapping/editing, upstream change detection and compatibility diagnostics remain open.
10. Complete UI build, type checks, browser/e2e tests, full graphical node/edge/port editor, impact analysis, robust source preservation and operator approval workflow.
11. OpenTelemetry spans/metrics/export, quotas and capacity controls, full audit history, release acceptance/e2e scenarios.
12. Execute and check the formal model under TLC/Alloy, rather than keeping an unchecked design sketch.

## Acceptance criteria for complete v1

- Two external MCP servers are connected over HTTP and exposed through one curated surface.
- A virtual tool composes multiple upstream calls, branches, joins, and invokes both an HTTP Decision API, a PII scrubber and an MCP-based LLM agent.
- A denied decision prevents any protected effect or exposure. Data classified as protected cannot cross a prohibited trust boundary, including in error paths and traces.
- A lost response to an external write does not cause duplicate action; Regate read-backs/reconciles or durably awaits an operator decision.
- An admin agent can inspect, patch, validate, test, review semantic diff, activate and roll back config using only management MCP + supplied skills.
- A user can achieve the same flow in the Svelte Flow UI, preserving unsupported HCL fragments.
- Updated upstream schemas are detected, breaking changes isolated; in-flight calls retain their original immutable revision.
- Tasks survive restart, subscription notifications work, concurrency/authorization invariants pass formal and load tests.
- Standard MCP conformance, browser integration, race/security and production acceptance suites all pass.

#### Compiler milestone: port schemas and source-safe graph edits

- JSON Schema 2020-12 input/output contracts are compiled before activation.
- Surface and node contracts are checked at the external effect boundary;
  dynamic remote data is refined at runtime.
- Basic static producer/consumer incompatibilities are rejected; full JSON
  Schema subsumption, arbitrary CEL access paths and remote schema provenance
  cannot be statically proven and remain runtime/policy responsibilities.
- Structural graph patching preserves untouched nested comments, attributes
  and blocks; structural deletion removes the deleted element's own contents.
- Remote JSON Schema loading is explicitly forbidden to prevent SSRF at
  compilation. Contract schemas are embedded within HCL.

#### Official SDK upstream milestone

- Upstream tools/list pagination, tools/call and resources/read go through the official MCP Go SDK; legacy initialize and modern server/discover are delegated to the SDK. No stdio transport is supported.
- Each upstream operation has an independent SDK session. This is intentionally conservative: session initialization per operation is slower, but stale connections and hidden write retries are avoided; production connection pooling remains open.
- Outbound DNS-pinned egress and redirect blocking are preserved; token-env credentials attach only through the guarded HTTP client. This does not implement OAuth.
- Incoming standard public/admin MCP methods are SDK-backed in PR #15; custom methods for skills and task management are SDK-registered. A narrow existing wire fallback is kept for versionless Regate discovery and task-start responses (`resultType: task`) until the SDK models that extension. This does **not** prove full MCP conformance or production-readiness.

#### Official SDK inbound milestone (PR #15)

- Public/admin HTTP endpoints use official Go MCP SDK streamable HTTP with stateless requests and JSON responses, plus SDK-managed SSE `subscriptions/listen` streams.
- Tools, resources, prompts and admin skills are registered with the SDK; custom `skills/*` and `tasks/*` methods remain typed SDK extension handlers. Strict modern `Mcp-Method`/`Mcp-Name` requirements remain enforced, and admin authorization stays outside the protocol handler.
- Activation/rollback builds a new SDK catalog before committing the revision. A catalog switch leaves the old revision intact for in-flight handlers and notifies existing subscriptions through non-destructive descriptor re-registration.
- Existing Regate HTTP clients that omit `Accept` remain compatible; versionless `server/discover` retains a compatibility handler. The SDK does not currently have a typed `resultType: task` response, so task-start is still narrowly bridged through the previous dispatcher.
- Remaining limitations: no full independent protocol conformance suite, browser/runtime e2e, persistent resume of SSE sessions, production OAuth, SQLite effect durability, or tested cross-instance subscription fan-out.

#### Compiler opaque-draft editing (issue #3)

- HCL2 syntax-only `ParseForEdit` retains unrecognized attributes and nested blocks, including expressions that are not statically evaluable.
- Graph structural edits, known attribute updates and insertions preserve unrepresented opaque text byte-for-byte; edits to unknown fields or new unsupported blocks are rejected rather than silently applied or lost.
- The management draft patch accepts syntactically valid HCL while `validate` and `activate` continue to strictly reject unsupported executable HCL.
- Negative and source-preservation round-trip tests cover unknown expressions/blocks, source edits, invalid syntax and failed activation. Browser-level round trip is tracked in #8.

#### MCP full catalog and conformance milestone (PR #17)

- Admin upstream refresh captures all advertised tools, resources, resource templates and prompts in a single negotiated SDK session, with bounded pagination and strict cursor checks.
- Catalog entries include the configuration revision; activation/rollback invalidate cache and stale responses never repopulate it. Failed refreshes remain errors.
- Independent raw HTTP transport tests cover modern/legacy negotiation, standard catalog methods, skills, bad headers/methods, request-size rejection and cancellation before protected external effects.
- Remaining: external-provider compatibility matrix, extended task ownership/recovery across instances, durable SSE resumption, OAuth security, performance and load/chaos testing.
